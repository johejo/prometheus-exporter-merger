package main

import (
	"bufio"
	"bytes"
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
	httpClient               = sync.OnceValue(initHTTPClient)
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

func initHTTPClient() *http.Client {
	return &http.Client{}
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

func handler(cfg ListenerConfig) http.HandlerFunc {
	extraLabels := make(map[string][]byte, len(cfg.Exporters))
	for name, e := range cfg.Exporters {
		extraLabels[name] = []byte(strings.Join(append(
			mapToSliceLabels(e.Labels),
			mapToSliceLabels(cfg.CommonLabels)...,
		), ","))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		type payload struct {
			extra []byte
			body  io.ReadCloser
		}
		payloads := make(chan *payload, len(cfg.Exporters))

		go func() {
			defer close(payloads)
			var wg sync.WaitGroup
			wg.Add(len(cfg.Exporters))
			for name, e := range cfg.Exporters {
				go func() {
					defer wg.Done()
					slog.Debug("start fetching", "name", name, "uri", e.URI)
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URI, nil)
					if err != nil {
						slog.Error(err.Error())
						return
					}
					resp, err := httpClient().Do(req)
					if err != nil {
						slog.Error(err.Error())
						return
					}
					slog.Debug("finish fetching", "name", name)
					payloads <- &payload{extra: extraLabels[name], body: resp.Body}
				}()
			}
			wg.Wait()
		}()

		bw := bufio.NewWriter(w)
		for p := range payloads {
			slog.Debug("start copying body with merging labels")
			if err := copyBody(bw, p.body, p.extra); err != nil {
				slog.Error(err.Error())
				return
			}
			slog.Debug("finish copying body")
		}
		if err := bw.Flush(); err != nil {
			slog.Error(err.Error())
		}
	}
}

func mapToSliceLabels(m map[string]string) []string {
	s := make([]string, 0, len(m))
	for k, v := range m {
		s = append(s, fmt.Sprintf(`%s="%s"`, k, v))
	}
	slices.Sort(s) // map iteration order would make the output unstable
	return s
}

func copyBody(w *bufio.Writer, body io.ReadCloser, extraLabels []byte) error {
	defer body.Close()

	r := bufio.NewReader(body)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if writeErr := writeLine(w, line, extraLabels); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// writeLine copies line to w, splicing extraLabels into its label set when the
// line is a sample. Everything else - comments, blank lines, anything that does
// not parse - is passed through untouched.
func writeLine(w *bufio.Writer, line, extraLabels []byte) error {
	at, prefix, suffix, ok := labelInsertPoint(line)
	if len(extraLabels) == 0 || !ok {
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
		return
	}

	switch line[nameEnd] {
	case ' ', '\t':
		if !hasSampleValue(line, nameEnd) {
			return
		}
		return nameEnd, "{", "}", true
	case '{':
		labelsEnd := labelSetEnd(line, nameEnd)
		if labelsEnd < 0 || labelsEnd+1 >= len(line) || (line[labelsEnd+1] != ' ' && line[labelsEnd+1] != '\t') {
			return
		}
		if !hasSampleValue(line, labelsEnd+1) {
			return
		}
		if len(bytes.TrimSpace(line[nameEnd+1:labelsEnd])) > 0 {
			prefix = ","
		}
		return labelsEnd, prefix, "", true
	default:
		return
	}
}

func hasSampleValue(line []byte, valueStart int) bool {
	value := bytes.TrimLeft(line[valueStart:], " \t")
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
