package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/goccy/go-yaml"
)

var (
	httpClient               = func() *http.Client { return http.DefaultClient }
	logLevel                 = flag.String("log-level", "info", "logging level: debug, info, warn, error")
	config                   = flag.String("config", "config.yaml", "configuration file path")
	expandEnv                = flag.Bool("expand-env", false, "expand environment variables in config")
	selfMetricsAddress       = flag.String("self-metrics-address", ":9716", "listen address for self metrics")
	selfMetricsExposeMetdata = flag.Bool("self-metrics-expose-metadata", true, "expose self metrics metadata")
)

func main() {
	flag.Parse()

	initLogger(*logLevel)

	cfg, err := loadConfig(*config, *expandEnv)
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	if err := validateConfig(*cfg, *selfMetricsAddress); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	slog.Info("config loaded", "config", *config)

	var wg sync.WaitGroup
	wg.Add(len(*cfg))
	for name, lc := range *cfg {
		go func() {
			defer wg.Done()
			lc.name = name
			slog.Info("start", "name", name, "address", lc.Address)
			if err := serve(lc); err != nil {
				slog.Error(err.Error())
			}
		}()
	}
	wg.Add(1)
	go func() {
		slog.Info("listening self metrics", "address", *selfMetricsAddress)
		mux := http.NewServeMux()
		metrics.ExposeMetadata(*selfMetricsExposeMetdata)
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			metrics.WritePrometheus(w, true)
		})
		if err := http.ListenAndServe(*selfMetricsAddress, mux); err != nil {
			slog.Error(err.Error())
		}
	}()
	wg.Wait()
}

type Config map[string]ListenerConfig

type ListenerConfig struct {
	Address      string              `json:"address" yaml:"address"`
	Path         string              `json:"path" yaml:"path"`
	Exporters    map[string]Exporter `json:"exporters" yaml:"exporters"`
	CommonLabels map[string]string   `json:"commonLabels" yaml:"commonLabels"`
	name         string
}

type Exporter struct {
	URI     string            `json:"uri" yaml:"uri"`
	Timeout time.Duration     `json:"timeout" yaml:"timeout"`
	Labels  map[string]string `json:"labels" yaml:"labels"`
}

func initLogger(loglevel string) {
	slogLevel := slog.LevelInfo
	switch strings.ToLower(loglevel) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     slogLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				if source, ok := a.Value.Any().(*slog.Source); ok {
					source.File = filepath.Base(source.File)
				}
			}
			return a
		}},
	)))
}

func loadConfig(config string, expandEnv bool) (*Config, error) {
	b, err := os.ReadFile(config)
	if err != nil {
		return nil, err
	}

	if expandEnv {
		b = []byte(os.ExpandEnv(string(b)))
	}

	var cfg Config
	if err := yaml.UnmarshalWithOptions(b, &cfg, yaml.DisallowUnknownField()); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateConfig(cfg Config, selfMetricsAddress string) error {
	var errs []error
	if len(cfg) == 0 {
		errs = append(errs, errors.New("at least one listener is required"))
	}
	if selfMetricsAddress == "" {
		errs = append(errs, errors.New("self metrics address must not be empty"))
	}

	addresses := make(map[string]string, len(cfg))
	if selfMetricsAddress != "" {
		addresses[selfMetricsAddress] = "self metrics"
	}
	for _, name := range slices.Sorted(maps.Keys(cfg)) {
		listener := cfg[name]
		location := fmt.Sprintf("listener %q", name)
		if name == "" {
			errs = append(errs, errors.New("listener name must not be empty"))
		}
		if listener.Address == "" {
			errs = append(errs, fmt.Errorf("%s: address must not be empty", location))
		} else if previous, ok := addresses[listener.Address]; ok {
			errs = append(errs, fmt.Errorf("%s: address %q is also used by %s", location, listener.Address, previous))
		} else {
			addresses[listener.Address] = location
		}
		if err := validateListenerPath(listener.Path); err != nil {
			errs = append(errs, fmt.Errorf("%s: path: %w", location, err))
		}
		if len(listener.Exporters) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one exporter is required", location))
		}
		for _, label := range slices.Sorted(maps.Keys(listener.CommonLabels)) {
			if !validLabelName(label) {
				errs = append(errs, fmt.Errorf("%s: commonLabels: invalid label name %q", location, label))
			}
		}

		for _, exporterName := range slices.Sorted(maps.Keys(listener.Exporters)) {
			exporter := listener.Exporters[exporterName]
			exporterLocation := fmt.Sprintf("%s: exporter %q", location, exporterName)
			if exporterName == "" {
				errs = append(errs, fmt.Errorf("%s: exporter name must not be empty", location))
			}
			if err := validateExporterURI(exporter.URI); err != nil {
				errs = append(errs, fmt.Errorf("%s: uri: %w", exporterLocation, err))
			}
			if exporter.Timeout < 0 {
				errs = append(errs, fmt.Errorf("%s: timeout must not be negative", exporterLocation))
			}
			for _, label := range slices.Sorted(maps.Keys(exporter.Labels)) {
				if !validLabelName(label) {
					errs = append(errs, fmt.Errorf("%s: labels: invalid label name %q", exporterLocation, label))
				}
				if _, duplicate := listener.CommonLabels[label]; duplicate {
					errs = append(errs, fmt.Errorf("%s: label %q is also defined in commonLabels", exporterLocation, label))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	return nil
}

func validateExporterURI(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("invalid URI: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("host must not be empty")
	}
	return nil
}

func validateListenerPath(path string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid HTTP pattern: %v", recovered)
		}
	}()
	if !strings.HasPrefix(path, "/") {
		return errors.New("must start with /")
	}
	http.NewServeMux().Handle(path, http.NotFoundHandler()) // panics on malformed patterns
	return nil
}

func validLabelName(name string) bool {
	return name != "" && metricNameEnd([]byte(name)) == len(name) && !strings.Contains(name, ":")
}

func serve(cfg ListenerConfig) error {
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, handler(cfg))
	return http.ListenAndServe(cfg.Address, mux)
}

type target struct {
	name     string
	uri      string
	timeout  time.Duration
	extra    []byte
	requests *metrics.Counter
	failures *metrics.Counter
	duration *metrics.PrometheusHistogram
}

type fetchResult struct {
	target  target
	body    io.ReadCloser
	cancel  context.CancelFunc
	started time.Time
	err     error
}

func handler(cfg ListenerConfig) http.HandlerFunc {
	targets := make([]target, 0, len(cfg.Exporters))
	for name, e := range cfg.Exporters {
		metricLabels := fmt.Sprintf(
			`listener="%s",upstream="%s"`,
			labelValueEscaper.Replace(cfg.name),
			labelValueEscaper.Replace(name),
		)
		targets = append(targets, target{
			name:    name,
			uri:     e.URI,
			timeout: e.Timeout,
			extra: []byte(strings.Join(append(
				mapToSliceLabels(e.Labels),
				mapToSliceLabels(cfg.CommonLabels)...,
			), ",")),
			requests: metrics.GetOrCreateCounter(
				"exporter_merger_upstream_requests_total{" + metricLabels + "}",
			),
			failures: metrics.GetOrCreateCounter(
				"exporter_merger_upstream_request_failures_total{" + metricLabels + "}",
			),
			duration: metrics.GetOrCreatePrometheusHistogram(
				"exporter_merger_upstream_request_duration_seconds{" + metricLabels + "}",
			),
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		results := make(chan fetchResult, len(targets))
		for _, t := range targets {
			go fetch(ctx, t, results)
		}

		// Do not copy any body until every upstream has returned its headers. This
		// lets us return an error instead of a partial successful exposition.
		fetched := make([]fetchResult, 0, len(targets))
		failed := false
		for range targets {
			result := <-results
			if result.err != nil {
				// The first failure makes the merged response impossible. Cancel
				// outstanding requests immediately, but don't blame upstreams which
				// merely observed that cancellation.
				if !failed || !errors.Is(result.err, context.Canceled) {
					result.target.failures.Inc()
				}
				slog.Error("failed fetching upstream", "name", result.target.name, "uri", result.target.uri, "error", result.err)
				if !failed {
					failed = true
					cancel()
				}
			}
			fetched = append(fetched, result)
		}
		if failed {
			for i := range fetched {
				fetched[i].close()
			}
			http.Error(w, "failed to fetch upstream exporters", http.StatusBadGateway)
			return
		}

		bw := bufio.NewWriter(w)
		seenMetadata := make(map[string]struct{})
		var copyErr error
		for i := range fetched {
			result := &fetched[i]
			if copyErr == nil {
				slog.Debug("start copying body with merging labels", "name", result.target.name)
				if copyErr = copyBody(bw, result.body, result.target.extra, seenMetadata); copyErr != nil {
					result.target.failures.Inc()
					slog.Error("failed reading upstream response", "name", result.target.name, "uri", result.target.uri, "error", copyErr)
					cancel()
				} else {
					slog.Debug("finish copying body", "name", result.target.name)
				}
			}
			result.close()
		}
		if copyErr != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			slog.Error(err.Error())
		}
	}
}

func fetch(ctx context.Context, t target, results chan<- fetchResult) {
	result := fetchResult{target: t, started: time.Now()}
	requestCtx := ctx
	result.cancel = func() {}
	if t.timeout > 0 {
		requestCtx, result.cancel = context.WithTimeout(ctx, t.timeout)
	}

	slog.Debug("start fetching", "name", t.name, "uri", t.uri, "timeout", t.timeout)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, t.uri, nil)
	if err != nil {
		result.err = err
		results <- result
		return
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		result.err = err
		results <- result
		return
	}
	result.body = resp.Body
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.err = fmt.Errorf("unexpected HTTP status %s", resp.Status)
	} else {
		slog.Debug("received upstream headers", "name", t.name, "status", resp.Status)
	}
	results <- result
}

func (r *fetchResult) close() {
	if r.body != nil {
		if err := r.body.Close(); err != nil {
			slog.Debug("failed closing upstream response", "name", r.target.name, "error", err)
		}
	}
	r.cancel()
	r.target.requests.Inc()
	r.target.duration.UpdateDuration(r.started)
}

var labelValueEscaper = strings.NewReplacer(
	`\`, `\\`,
	"\n", `\n`,
	`"`, `\"`,
)

func mapToSliceLabels(m map[string]string) []string {
	s := make([]string, 0, len(m))
	for k, v := range m {
		s = append(s, fmt.Sprintf(`%s="%s"`, k, labelValueEscaper.Replace(v)))
	}
	slices.Sort(s) // map iteration order would make the output unstable
	return s
}

// readBufferSize is large enough that a whole exposition line practically always
// fits, which keeps copyBody on its zero-copy path.
const readBufferSize = 64 << 10

// seenMetadata is shared across bodies so the first HELP and TYPE definitions win.
func copyBody(w *bufio.Writer, body io.Reader, extraLabels []byte, seenMetadata map[string]struct{}) error {
	r := bufio.NewReaderSize(body, readBufferSize)
	// ReadSlice returns a view into r's buffer, so no per-line allocation. Only a
	// line too long to fit gets assembled into joined, which is reused after that.
	var joined []byte
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			joined = append(joined[:0], line...)
			for err == bufio.ErrBufferFull {
				line, err = r.ReadSlice('\n')
				joined = append(joined, line...)
			}
			line = joined
		}
		if len(line) > 0 && !duplicateMetadata(seenMetadata, line) {
			if err := writeLine(w, line, extraLabels); err != nil {
				return err
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func duplicateMetadata(seen map[string]struct{}, line []byte) bool {
	key, ok := parseMetadataKey(line)
	if !ok {
		return false
	}
	// Looking up a []byte in a map[string]T does not copy the key, so only a
	// first sighting allocates.
	if _, duplicate := seen[string(key)]; duplicate {
		return true
	}
	seen[string(key)] = struct{}{}
	return false
}

func parseMetadataKey(line []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(line, []byte("# "))
	if !ok {
		return nil, false
	}
	directive, metric, ok := bytes.Cut(rest, []byte(" "))
	if !ok || string(directive) != "HELP" && string(directive) != "TYPE" {
		return nil, false
	}
	metricEnd := bytes.IndexAny(metric, " \t\r\n")
	if metricEnd <= 0 {
		return nil, false
	}
	return rest[:len(directive)+1+metricEnd], true
}

func writeLine(w *bufio.Writer, line, extraLabels []byte) error {
	if len(extraLabels) == 0 {
		_, err := w.Write(line)
		return err
	}
	at, prefix, suffix, ok := labelInsertPoint(line)
	if !ok {
		_, err := w.Write(line)
		return err
	}
	// bufio.Writer records the first error and fails every later write, so only
	// the final one needs checking.
	w.Write(line[:at])
	w.WriteString(prefix)
	w.Write(extraLabels)
	w.WriteString(suffix)
	_, err := w.Write(line[at:])
	return err
}

func labelInsertPoint(line []byte) (at int, prefix, suffix string, ok bool) {
	nameEnd := metricNameEnd(line)
	if nameEnd == 0 || nameEnd == len(line) {
		return 0, "", "", false
	}

	switch line[nameEnd] {
	case ' ', '\t':
		if !hasSampleValue(line, nameEnd) {
			return 0, "", "", false
		}
		return nameEnd, "{", "}", true
	case '{':
		labelsEnd := labelSetEnd(line, nameEnd)
		if labelsEnd < 0 || !hasSampleValue(line, labelsEnd+1) {
			return 0, "", "", false
		}
		if len(bytes.TrimSpace(line[nameEnd+1:labelsEnd])) > 0 {
			prefix = ","
		}
		return labelsEnd, prefix, "", true
	default:
		return 0, "", "", false
	}
}

func hasSampleValue(line []byte, valueStart int) bool {
	rest := line[valueStart:]
	if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	value := bytes.TrimLeft(rest, " \t")
	if i := bytes.IndexAny(value, " \t\r\n"); i >= 0 {
		value = value[:i]
	}
	if len(value) == 0 {
		return false
	}
	_, err := strconv.ParseFloat(string(value), 64)
	return err == nil
}

func metricNameEnd(line []byte) int {
	if len(line) == 0 || !isMetricNameStart(line[0]) {
		return 0
	}
	for i := 1; i < len(line); i++ {
		c := line[i]
		if !(isMetricNameStart(c) || c >= '0' && c <= '9') {
			return i
		}
	}
	return len(line)
}

func isMetricNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == ':'
}

func labelSetEnd(line []byte, labelsStart int) int {
	inQuotes := false
	for i := labelsStart + 1; i < len(line); i++ {
		switch {
		case inQuotes && line[i] == '\\':
			i++ // skip the escaped byte
		case line[i] == '"':
			inQuotes = !inQuotes
		case !inQuotes && line[i] == '}':
			return i
		}
	}
	return -1
}
