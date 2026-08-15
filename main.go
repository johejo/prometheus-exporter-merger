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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/goccy/go-yaml"
)

const (
	shutdownTimeout = 10 * time.Second
	selfMetricsName = "self metrics"
)

var (
	httpClient               = http.DefaultClient
	logLevel                 = flag.String("log-level", "info", "logging level: debug, info, warn, error")
	config                   = flag.String("config", "config.yaml", "configuration file path")
	expandEnv                = flag.Bool("expand-env", false, "expand environment variables in config")
	selfMetricsAddress       = flag.String("self-metrics-address", ":9716", "listen address for self metrics")
	selfMetricsExposeMetdata = flag.Bool("self-metrics-expose-metadata", true, "expose self metrics metadata")
)

func main() {
	flag.Parse()

	initLogger(*logLevel)
	metrics.ExposeMetadata(*selfMetricsExposeMetdata)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *cfg, *selfMetricsAddress); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

type Config map[string]ListenerConfig

type ListenerConfig struct {
	Address      string              `yaml:"address"`
	Path         string              `yaml:"path"`
	Exporters    map[string]Exporter `yaml:"exporters"`
	CommonLabels map[string]string   `yaml:"commonLabels"`
}

type Exporter struct {
	URI     string            `yaml:"uri"`
	Timeout time.Duration     `yaml:"timeout"`
	Labels  map[string]string `yaml:"labels"`
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
	addresses := make(map[string]string, len(cfg))
	if selfMetricsAddress == "" {
		errs = append(errs, errors.New("self metrics address must not be empty"))
	} else {
		addresses[selfMetricsAddress] = selfMetricsName
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

type listenerServer struct {
	name     string
	server   *http.Server
	listener net.Listener
}

func run(ctx context.Context, cfg Config, selfMetricsAddress string) error {
	type listenerSpec struct {
		name    string
		address string
		path    string
		handler http.Handler
	}
	specs := make([]listenerSpec, 0, len(cfg)+1)
	for name, listenerCfg := range cfg {
		specs = append(specs, listenerSpec{name, listenerCfg.Address, listenerCfg.Path, handler(name, listenerCfg)})
	}
	specs = append(specs, listenerSpec{selfMetricsName, selfMetricsAddress, "/metrics",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			metrics.WritePrometheus(w, true)
		})})

	servers := make([]listenerServer, 0, len(specs))
	for _, spec := range specs {
		mux := http.NewServeMux()
		mux.Handle(spec.path, spec.handler)
		listener, err := net.Listen("tcp", spec.address)
		if err != nil {
			for _, server := range servers {
				_ = server.listener.Close()
			}
			return fmt.Errorf("listen %s on %s: %w", spec.name, spec.address, err)
		}
		servers = append(servers, listenerServer{
			name:     spec.name,
			server:   &http.Server{Handler: mux},
			listener: listener,
		})
	}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(servers))
	for _, server := range servers {
		go func() {
			slog.Info("listening", "name", server.name, "address", server.listener.Addr())
			results <- serveServer(serveCtx, server)
		}()
	}

	// One listener failing makes the process useless, so stop the others too, but
	// report every failure rather than only the one that arrived first.
	var errs []error
	for range servers {
		if err := <-results; err != nil {
			errs = append(errs, err)
			cancel()
		}
	}
	return errors.Join(errs...)
}

func serveServer(ctx context.Context, server listenerServer) error {
	// Serve reports ErrServerClosed once Shutdown or Close has been called.
	wrapServe := func(err error) error {
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve %s: %w", server.name, err)
	}

	errs := make(chan error, 1)
	go func() {
		errs <- server.server.Serve(server.listener)
	}()

	select {
	case err := <-errs:
		return wrapServe(err)
	case <-ctx.Done():
	}

	slog.Info("shutting down", "name", server.name)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := server.server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		// Shutdown leaves active connections open when its deadline expires.
		_ = server.server.Close()
		shutdownErr = fmt.Errorf("shut down %s: %w", server.name, shutdownErr)
	}
	return errors.Join(shutdownErr, wrapServe(<-errs))
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

func handler(name string, cfg ListenerConfig) http.HandlerFunc {
	targets := make([]target, 0, len(cfg.Exporters))
	for exporterName, e := range cfg.Exporters {
		metricLabels := formatLabel("listener", name) + "," + formatLabel("upstream", exporterName)
		targets = append(targets, target{
			name:    exporterName,
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
			go func() { results <- fetch(ctx, t) }()
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
			closeAll(fetched)
			http.Error(w, "failed to fetch upstream exporters", http.StatusBadGateway)
			return
		}

		bw := writerPool.Get().(*bufio.Writer)
		defer func() {
			bw.Reset(nil) // drop the ResponseWriter and any unflushed bytes
			writerPool.Put(bw)
		}()
		bw.Reset(w)
		seenMetadata := make(map[string]struct{})
		for i := range fetched {
			result := &fetched[i]
			slog.Debug("start copying body with merging labels", "name", result.target.name)
			err := copyBody(bw, result.body, result.target.extra, seenMetadata)
			result.close()
			if err != nil {
				result.target.failures.Inc()
				slog.Error("failed reading upstream response", "name", result.target.name, "uri", result.target.uri, "error", err)
				cancel()
				closeAll(fetched[i+1:])
				return
			}
			slog.Debug("finish copying body", "name", result.target.name)
		}
		if err := bw.Flush(); err != nil {
			slog.Error(err.Error())
		}
	}
}

func fetch(ctx context.Context, t target) fetchResult {
	result := fetchResult{target: t, started: time.Now()}
	requestCtx := ctx
	if t.timeout > 0 {
		requestCtx, result.cancel = context.WithTimeout(ctx, t.timeout)
	}

	slog.Debug("start fetching", "name", t.name, "uri", t.uri, "timeout", t.timeout)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, t.uri, nil)
	if err != nil {
		result.err = err
		return result
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		result.err = err
		return result
	}
	result.body = resp.Body
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.err = fmt.Errorf("unexpected HTTP status %s", resp.Status)
	} else {
		slog.Debug("received upstream headers", "name", t.name, "status", resp.Status)
	}
	return result
}

func closeAll(fetched []fetchResult) {
	for i := range fetched {
		fetched[i].close()
	}
}

func (r *fetchResult) close() {
	if r.body != nil {
		if err := r.body.Close(); err != nil {
			slog.Debug("failed closing upstream response", "name", r.target.name, "error", err)
		}
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.target.requests.Inc()
	r.target.duration.UpdateDuration(r.started)
}

var labelValueEscaper = strings.NewReplacer(
	`\`, `\\`,
	"\n", `\n`,
	`"`, `\"`,
)

func formatLabel(name, value string) string {
	return fmt.Sprintf(`%s="%s"`, name, labelValueEscaper.Replace(value))
}

func mapToSliceLabels(m map[string]string) []string {
	s := make([]string, 0, len(m))
	for k, v := range m {
		s = append(s, formatLabel(k, v))
	}
	slices.Sort(s) // map iteration order would make the output unstable
	return s
}

// bufferSize is large enough that a whole exposition line practically always
// fits, which keeps copyBody on its zero-copy path.
const bufferSize = 64 << 10

// Buffers are pooled because a merge allocates one reader per upstream and one
// writer per request, which would otherwise dominate the garbage a scrape makes.
var (
	readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, bufferSize) }}
	writerPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, bufferSize) }}
)

// seenMetadata is shared across bodies so the first HELP and TYPE definitions win.
func copyBody(w *bufio.Writer, body io.Reader, extraLabels []byte, seenMetadata map[string]struct{}) error {
	r := readerPool.Get().(*bufio.Reader)
	defer func() {
		r.Reset(nil) // do not let the pool keep the body alive
		readerPool.Put(r)
	}()
	r.Reset(body)
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
