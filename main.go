package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

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

	slog.Info("config loaded", "config", *config)

	var wg sync.WaitGroup
	wg.Add(len(*cfg))
	for name, lc := range *cfg {
		go func() {
			defer wg.Done()
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
			metrics.WriteProcessMetrics(w)
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
}

type Exporter struct {
	URI    string            `json:"uri" yaml:"uri"`
	Labels map[string]string `json:"labels" yaml:"labels"`
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
	var unmarshal func(b []byte, dst any) error
	switch filepath.Ext(config) {
	case ".json":
		unmarshal = json.Unmarshal
	case ".yaml", ".yml":
		unmarshal = yaml.Unmarshal
	default:
		return nil, fmt.Errorf("unsupported file %s", config)
	}

	if err := unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func serve(cfg ListenerConfig) error {
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, handler(cfg))
	return http.ListenAndServe(cfg.Address, mux)
}

// target is an exporter resolved against its listener's configuration, with the
// labels to splice into its samples precomputed once at handler construction.
type target struct {
	name  string
	uri   string
	extra []byte
}

// payload is a fetched exporter response on its way to the merged output. The
// receiver owns body and is responsible for closing it.
type payload struct {
	extra []byte
	body  io.ReadCloser
}

func handler(cfg ListenerConfig) http.HandlerFunc {
	targets := make([]target, 0, len(cfg.Exporters))
	for name, e := range cfg.Exporters {
		targets = append(targets, target{
			name: name,
			uri:  e.URI,
			extra: []byte(strings.Join(append(
				mapToSliceLabels(e.Labels),
				mapToSliceLabels(cfg.CommonLabels)...,
			), ",")),
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		payloads := make(chan payload, len(targets))

		go func() {
			defer close(payloads)
			var wg sync.WaitGroup
			wg.Add(len(targets))
			for _, t := range targets {
				go func() {
					defer wg.Done()
					slog.Debug("start fetching", "name", t.name, "uri", t.uri)
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.uri, nil)
					if err != nil {
						slog.Error(err.Error())
						return
					}
					resp, err := httpClient().Do(req)
					if err != nil {
						slog.Error(err.Error())
						return
					}
					slog.Debug("finish fetching", "name", t.name)
					select {
					case payloads <- payload{extra: t.extra, body: resp.Body}:
					case <-ctx.Done():
						resp.Body.Close() // the hand-off failed, so ownership never transferred
					}
				}()
			}
			wg.Wait()
		}()

		bw := bufio.NewWriter(w)
		var copyErr error
		for p := range payloads {
			if copyErr == nil {
				slog.Debug("start copying body with merging labels")
				if copyErr = copyBody(bw, p.body, p.extra); copyErr != nil {
					slog.Error(copyErr.Error())
					cancel() // stop in-flight fetches; whatever is already on its way is drained here
				} else {
					slog.Debug("finish copying body")
				}
			}
			p.body.Close()
		}
		if copyErr != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			slog.Error(err.Error())
		}
	}
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

// copyBody streams body to w, splicing extraLabels into every sample line. It
// does not close body; the caller owns it.
func copyBody(w *bufio.Writer, body io.Reader, extraLabels []byte) error {
	if len(extraLabels) == 0 {
		_, err := io.Copy(w, body)
		return err
	}

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
		if len(line) > 0 {
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

// writeLine copies line to w, splicing the non-empty extraLabels into its label
// set when the line is a sample. Everything else - comments, blank lines,
// anything that does not parse - is passed through untouched.
func writeLine(w *bufio.Writer, line, extraLabels []byte) error {
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

// labelInsertPoint locates where extra labels belong in a sample line: at is the
// offset to splice at, and prefix/suffix are the delimiters to write around them
// ("{" and "}" for a sample with no label set, "," and "" when merging into an
// existing one). ok is false when line is not a sample.
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

// hasSampleValue reports whether a numeric sample value follows valueStart,
// which must be the offset of the whitespace separating the value from the
// metric name or label set.
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
