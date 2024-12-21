package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/CAFxX/httpcompression"
	"github.com/VictoriaMetrics/metrics"
	"github.com/goccy/go-yaml"
	"github.com/icholy/replace"
	"github.com/klauspost/compress/gzhttp"
	"golang.org/x/text/transform"
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
		if err := http.ListenAndServe(*selfMetricsAddress, gzhttp.GzipHandler(mux)); err != nil {
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
	return &http.Client{
		Transport: gzhttp.Transport(http.DefaultTransport),
	}
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
	compress, err := httpcompression.DefaultAdapter()
	if err != nil {
		return err
	}
	return http.ListenAndServe(cfg.Address, compress(mux))
}

func handler(cfg ListenerConfig) http.HandlerFunc {
	transformers := make(map[string]transform.Transformer)
	for name, e := range cfg.Exporters {
		transformers[name] = transformer(append(
			mapToSliceLabels(e.Labels),
			mapToSliceLabels(cfg.CommonLabels)...,
		))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		type Payload struct {
			Name string
			Body io.ReadCloser
		}
		payload := make(chan *Payload, len(cfg.Exporters))

		go func() {
			defer close(payload)
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
					payload <- &Payload{Name: name, Body: resp.Body}
				}()
			}
			wg.Wait()
		}()

		for p := range payload {
			slog.Debug("start copying body with merging labels")
			if err := copyBody(w, p.Body, transformers[p.Name]); err != nil {
				slog.Error(err.Error())
				return
			}
			slog.Debug("finish copying body")
		}
	}
}

func mapToSliceLabels(m map[string]string) []string {
	s := make([]string, 0, len(m))
	for k, v := range m {
		s = append(s, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return s
}

var (
	metricRegex = regexp.MustCompile(`\n(?<name>[a-zA-Z_:][a-zA-Z0-9_:]*)(?<labels>\{[^\}].*\})?\s(?<sample>[0-9eE\.\+\-]+)`)
)

func transformer(labels []string) transform.Transformer {
	return replace.RegexpStringSubmatchFunc(metricRegex, mergeLabels(labels))
}

func mergeLabels(kv []string) func(s []string) string {
	return func(s []string) string {
		name := s[1]
		labels := s[2]
		sample := s[3]
		slog.Debug("parsing series", "name", name, "labels", labels, "sample", sample)
		if labels == "" && len(kv) == 0 {
			return "\n" + name + " " + sample
		}
		if labels == "" {
			labels = "{" + strings.Join(kv, ",") + "}"
		} else {
			labels = strings.TrimSuffix(labels, "}") + "," + strings.Join(kv, ",") + "}"
		}
		return "\n" + name + labels + " " + sample
	}
}

func copyBody(w io.Writer, body io.ReadCloser, transformer transform.Transformer) error {
	defer body.Close()
	_, err := io.Copy(w, transform.NewReader(body, transformer))
	return err
}
