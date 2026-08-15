package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/klauspost/compress/zstd"
)

func TestCurrentVersion(t *testing.T) {
	t.Run("ldflags version", func(t *testing.T) {
		original := version
		version = "v1.2.3"
		t.Cleanup(func() { version = original })

		if got, want := currentVersion(), "v1.2.3"; got != want {
			t.Fatalf("currentVersion() = %q, want %q", got, want)
		}
	})

	t.Run("build info fallback", func(t *testing.T) {
		original := version
		version = ""
		t.Cleanup(func() { version = original })

		buildInfo, ok := debug.ReadBuildInfo()
		want := "unknown"
		if ok && buildInfo.Main.Version != "" {
			want = buildInfo.Main.Version
		}
		if got := currentVersion(); got != want {
			t.Fatalf("currentVersion() = %q, want %q", got, want)
		}
	})
}

func TestLogFormat(t *testing.T) {
	if got, want := *logFormat, "text"; got != want {
		t.Fatalf("default log format = %q, want %q", got, want)
	}

	tests := []struct {
		name   string
		format string
		check  func(*testing.T, []byte)
	}{
		{
			name:   "text",
			format: "text",
			check: func(t *testing.T, output []byte) {
				t.Helper()
				got := string(output)
				for _, want := range []string{"level=INFO", `msg="format test"`, "answer=42"} {
					if !strings.Contains(got, want) {
						t.Fatalf("text log does not contain %q: %s", want, got)
					}
				}
			},
		},
		{
			name:   "json",
			format: "json",
			check: func(t *testing.T, output []byte) {
				t.Helper()
				var entry map[string]any
				if err := json.Unmarshal(output, &entry); err != nil {
					t.Fatalf("log is not valid JSON: %v\n%s", err, output)
				}
				if got, want := entry["level"], "INFO"; got != want {
					t.Errorf("level = %v, want %v", got, want)
				}
				if got, want := entry["msg"], "format test"; got != want {
					t.Errorf("msg = %v, want %v", got, want)
				}
				if got, want := entry["answer"], float64(42); got != want {
					t.Errorf("answer = %v, want %v", got, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalLogger := slog.Default()
			originalStderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				slog.SetDefault(originalLogger)
				os.Stderr = originalStderr
				_ = r.Close()
				_ = w.Close()
			})

			os.Stderr = w
			initLogger("info", tt.format)
			slog.Info("format test", "answer", 42)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			output, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, output)
		})
	}
}

func TestResponseCompression(t *testing.T) {
	wrapper, err := newCompressionWrapper()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("metric 1\n", 256)
	h := wrapper(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))

	t.Run("prefers gzip", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("Accept-Encoding", "gzip, zstd")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got, want := rec.Header().Get("Content-Encoding"), "gzip"; got != want {
			t.Fatalf("Content-Encoding: got %q, want %q", got, want)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if err := zr.Close(); err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Error("decompressed response differs from the original")
		}
	})

	t.Run("supports zstd", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("Accept-Encoding", "zstd")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got, want := rec.Header().Get("Content-Encoding"), "zstd"; got != want {
			t.Fatalf("Content-Encoding: got %q, want %q", got, want)
		}
		zr, err := zstd.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Error("decompressed response differs from the original")
		}
	})

	t.Run("identity", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding: got %q, want none", got)
		}
		if got := rec.Body.String(); got != body {
			t.Error("response differs from the original")
		}
	})
}

func TestServeServerGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "complete")
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := listenerServer{
		name:     "test",
		server:   &http.Server{Handler: handler},
		listener: listener,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveServer(ctx, server) }()

	requested := make(chan struct{})
	go func() {
		defer close(requested)
		client := &http.Client{Transport: &http.Transport{}}
		resp, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			t.Errorf("request failed: %v", err)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("failed reading response: %v", err)
			return
		}
		if got, want := string(body), "complete"; got != want {
			t.Errorf("response body: got %q, want %q", got, want)
		}
	}()

	select {
	case <-started:
	case <-requested:
		t.Fatal("request did not start")
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("server stopped before active request completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("request did not complete")
	}
	if err := <-done; err != nil {
		t.Fatalf("serveServer returned an error: %v", err)
	}
}

func TestHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "# HELP metric documentation\n# TYPE metric gauge\nmetric 1\n")
	}))
	defer upstream.Close()

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"first":  {URI: upstream.URL},
			"second": {URI: upstream.URL},
		},
		CommonLabels: map[string]string{"foo": "bar"},
	}
	rec := httptest.NewRecorder()
	handler("", cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := "# HELP metric documentation\n# TYPE metric gauge\nmetric{foo=\"bar\"} 1\nmetric{foo=\"bar\"} 1\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("response body: got %q, want %q", got, want)
	}
}

func TestHandlerClosesAllResponseBodiesAfterCopyError(t *testing.T) {
	readErr := errors.New("read failed")
	bodies := map[string]*trackingBody{
		"first":  {Reader: iotest.ErrReader(readErr)},
		"second": {Reader: iotest.ErrReader(readErr)},
	}

	// Hold every response until all of them are in flight, so the copy of the
	// first one fails while the others are still unclaimed.
	var arrived sync.WaitGroup
	arrived.Add(len(bodies))
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		arrived.Done()
		arrived.Wait()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       bodies[req.URL.Host],
			Request:    req,
		}, nil
	})

	stubHTTPClient(t, transport)

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"first":  {URI: "http://first"},
			"second": {URI: "http://second"},
		},
	}
	handler("", cfg).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	for name, body := range bodies {
		if !body.closed.Load() {
			t.Errorf("response body for %s was not closed", name)
		}
	}
}

func TestHandlerChecksAllHeadersBeforeWritingBodies(t *testing.T) {
	successBody := &trackingBody{Reader: strings.NewReader("must_not_be_merged 1\n")}
	failureBody := &trackingBody{Reader: strings.NewReader("upstream_error 1\n")}
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := successBody
		if req.URL.Host == "failure" {
			status = http.StatusServiceUnavailable
			body = failureBody
		}
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})

	stubHTTPClient(t, transport)

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"success": {URI: "http://success"},
			"failure": {URI: "http://failure"},
		},
	}
	failures := metrics.GetOrCreateCounter(
		`exporter_merger_upstream_request_failures_total{listener="header-check-test",upstream="failure"}`,
	)
	before := failures.Get()
	rec := httptest.NewRecorder()
	handler("header-check-test", cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if got, want := rec.Code, http.StatusBadGateway; got != want {
		t.Errorf("status: got %d, want %d", got, want)
	}
	if successBody.reads.Load() != 0 || failureBody.reads.Load() != 0 {
		t.Errorf("response body was read before all headers passed validation")
	}
	if !successBody.closed.Load() || !failureBody.closed.Load() {
		t.Errorf("all response bodies must be closed after header validation fails")
	}
	if got := failures.Get() - before; got != 1 {
		t.Errorf("failure counter increased by %d, want 1", got)
	}
}

func TestHandlerAppliesUpstreamTimeout(t *testing.T) {
	upstreamErr := make(chan error, 1)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		err := req.Context().Err()
		upstreamErr <- err
		return nil, err
	})
	stubHTTPClient(t, transport)

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"slow": {URI: "http://slow", Timeout: 10 * time.Millisecond},
		},
	}
	rec := httptest.NewRecorder()
	serveHTTPWithinDeadline(t, handler("timeout-test", cfg), rec)

	if got, want := rec.Code, http.StatusBadGateway; got != want {
		t.Errorf("status: got %d, want %d", got, want)
	}
	if err := <-upstreamErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("upstream error: got %v, want context deadline exceeded", err)
	}
}

func TestHandlerCancelsOtherUpstreamsAfterFailure(t *testing.T) {
	waitingStarted := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "failure" {
			<-waitingStarted
			return nil, errors.New("connection failed")
		}
		close(waitingStarted)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	stubHTTPClient(t, transport)

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"failure": {URI: "http://failure"},
			"waiting": {URI: "http://waiting"},
		},
	}
	waitingFailures := metrics.GetOrCreateCounter(
		`exporter_merger_upstream_request_failures_total{listener="cancel-test",upstream="waiting"}`,
	)
	before := waitingFailures.Get()
	rec := httptest.NewRecorder()
	serveHTTPWithinDeadline(t, handler("cancel-test", cfg), rec)

	if got, want := rec.Code, http.StatusBadGateway; got != want {
		t.Errorf("status: got %d, want %d", got, want)
	}
	if got := waitingFailures.Get() - before; got != 0 {
		t.Errorf("canceled sibling failure counter increased by %d, want 0", got)
	}
}

func TestLoadConfigParsesExporterTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`default:
  address: ":8080"
  path: /metrics
  exporters:
    node:
      uri: http://node/metrics
      timeout: 2.5s
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := (*cfg)["default"].Exporters["node"].Timeout, 2500*time.Millisecond; got != want {
		t.Errorf("timeout: got %s, want %s", got, want)
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`default:
  address: ":8080"
  path: /metrics
  unexpected: value
  exporters:
    node:
      uri: http://node/metrics
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadConfig(path, false); err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

func TestValidateConfig(t *testing.T) {
	valid := Config{
		"default": {
			Address:      ":8080",
			Path:         "/metrics",
			CommonLabels: map[string]string{"machine": "foo"},
			Exporters: map[string]Exporter{
				"node": {
					URI:     "http://node:9100/metrics",
					Timeout: 5 * time.Second,
					Labels:  map[string]string{"job": "node"},
				},
			},
		},
	}
	if err := validateConfig(valid, ":9716"); err != nil {
		t.Fatalf("valid config was rejected: %v", err)
	}

	invalid := Config{
		"broken": {
			Address:      ":9716",
			Path:         "metrics",
			CommonLabels: map[string]string{"bad-label": "value", "job": "common"},
			Exporters: map[string]Exporter{
				"node": {
					URI:     "ftp://node/metrics",
					Timeout: -time.Second,
					Labels:  map[string]string{"job": "exporter"},
				},
			},
		},
		"malformed_path": {
			Address: ":8081",
			Path:    "/metrics/{",
			Exporters: map[string]Exporter{
				"node": {URI: "http://node/metrics"},
			},
		},
	}
	err := validateConfig(invalid, ":9716")
	if err == nil {
		t.Fatal("invalid config was accepted")
	}
	for _, want := range []string{
		`address ":9716" is also used by self metrics`,
		`path: must start with /`,
		`invalid HTTP pattern`,
		`invalid label name "bad-label"`,
		`scheme must be http or https`,
		`timeout must not be negative`,
		`label "job" is also defined in commonLabels`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error %q does not contain %q", err, want)
		}
	}
}

func TestHandlerRecordsMetricsAfterConsumingUpstreamBody(t *testing.T) {
	const metricLabels = `listener="duration-test",upstream="success"`
	const durationCount = "exporter_merger_upstream_request_duration_seconds_count{" + metricLabels + "}"

	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	body := &blockingBody{
		trackingBody: trackingBody{Reader: strings.NewReader("metric 1\n")},
		started:      started,
		release:      release,
	}
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})
	stubHTTPClient(t, transport)

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"success": {URI: "http://success"},
		},
	}
	requests := metrics.GetOrCreateCounter("exporter_merger_upstream_requests_total{" + metricLabels + "}")
	requestsBefore := requests.Get()
	durationsBefore := metricValue(t, durationCount)
	assertRecorded := func(when string, want uint64) {
		t.Helper()
		if got := requests.Get() - requestsBefore; got != want {
			t.Errorf("request counter %s increased by %d, want %d", when, got, want)
		}
		if got := metricValue(t, durationCount) - durationsBefore; got != want {
			t.Errorf("request duration count %s increased by %d, want %d", when, got, want)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler("duration-test", cfg).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start consuming the upstream body")
	}
	assertRecorded("before consuming body", 0)

	releaseOnce()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after the upstream body was released")
	}
	if !body.closed.Load() {
		t.Error("upstream body was not closed")
	}
	assertRecorded("after consuming body", 1)
}

// metricValue reports the current value of the named self metric, or zero if
// it has not been registered yet.
func metricValue(t *testing.T, name string) uint64 {
	t.Helper()

	var output bytes.Buffer
	metrics.WritePrometheus(&output, false)
	for line := range strings.Lines(output.String()) {
		value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), name+" ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatalf("parse self metric %q: %v", line, err)
		}
		return n
	}
	return 0
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func stubHTTPClient(t *testing.T, rt http.RoundTripper) {
	original := httpClient
	httpClient = &http.Client{Transport: rt}
	t.Cleanup(func() { httpClient = original })
}

func serveHTTPWithinDeadline(t *testing.T, h http.Handler, rec *httptest.ResponseRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil).WithContext(ctx)
		h.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not complete within one second")
	}
}

type trackingBody struct {
	io.Reader
	reads  atomic.Int64
	closed atomic.Bool
}

func (r *trackingBody) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.Reader.Read(p)
}

func (r *trackingBody) Close() error {
	r.closed.Store(true)
	return nil
}

// blockingBody is a trackingBody that signals on started when the first read
// begins and blocks there until release is closed.
type blockingBody struct {
	trackingBody
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *blockingBody) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.trackingBody.Read(p)
}

func Test_mapToSliceLabelsEscapesValues(t *testing.T) {
	labels := mapToSliceLabels(map[string]string{
		"special": "quote \" backslash \\ newline\n",
	})
	if len(labels) != 1 {
		t.Fatalf("want one label, got %d", len(labels))
	}
	if want, got := `special="quote \" backslash \\ newline\n"`, labels[0]; got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func Test_copyBody(t *testing.T) {
	longValue := strings.Repeat("x", 2*bufferSize)
	exposition := strings.Join([]string{
		`# HELP go_gc_duration_seconds A summary of the pause duration of garbage collection cycles.`,
		`# TYPE go_gc_duration_seconds summary`,
		`go_gc_duration_seconds{quantile="0"} 5.871e-06`,
		`go_gc_duration_seconds_sum 0.464658525`,
	}, "\n")
	expositionWant := strings.Join([]string{
		`# HELP go_gc_duration_seconds A summary of the pause duration of garbage collection cycles.`,
		`# TYPE go_gc_duration_seconds summary`,
		`go_gc_duration_seconds{quantile="0",foo="bar"} 5.871e-06`,
		`go_gc_duration_seconds_sum{foo="bar"} 0.464658525`,
	}, "\n")

	tests := []struct {
		name        string
		origins     []string
		extraLabels string
		want        string
	}{
		{name: "exposition", origins: []string{exposition}, extraLabels: `foo="bar"`, want: expositionWant},
		{
			name:        "line longer than the read buffer",
			origins:     []string{fmt.Sprintf("metric{value=%q} 1\n", longValue)},
			extraLabels: `foo="bar"`,
			want:        fmt.Sprintf("metric{value=%q,foo=\"bar\"} 1\n", longValue),
		},
		{name: "no extra labels", origins: []string{exposition}, want: exposition},
		{
			name: "duplicate metadata across bodies",
			origins: []string{
				"# HELP shared first documentation\n# TYPE shared gauge\n# HELP first_only documentation\nfirst_only 1\nshared 1\n",
				"# HELP shared conflicting documentation\n# TYPE shared counter\n# HELP second_only documentation\nsecond_only 2\nshared 2\n",
			},
			want: "# HELP shared first documentation\n# TYPE shared gauge\n# HELP first_only documentation\nfirst_only 1\nshared 1\n" +
				"# HELP second_only documentation\nsecond_only 2\nshared 2\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := bufio.NewWriter(&buf)
			seen := make(map[string]struct{})
			for _, origin := range tt.origins {
				if err := copyBody(w, strings.NewReader(origin), []byte(tt.extraLabels), seen); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("want %q, got %q", tt.want, got)
			}
		})
	}
}

var writeLineTests = []struct {
	name string
	line string
	want string
}{
	{name: "without labels", line: "metric 1\n", want: `metric{foo="bar"} 1` + "\n"},
	{name: "with labels", line: `metric{existing="value"} NaN` + "\n", want: `metric{existing="value",foo="bar"} NaN` + "\n"},
	{name: "empty labels", line: "metric{} +Inf 123\n", want: `metric{foo="bar"} +Inf 123` + "\n"},
	{name: "closing brace in value", line: `metric{existing="}"} -Inf` + "\n", want: `metric{existing="}",foo="bar"} -Inf` + "\n"},
	{name: "help", line: "# HELP metric documentation\n", want: "# HELP metric documentation\n"},
	{name: "blank", line: "\n", want: "\n"},
	{name: "invalid", line: "not a sample\n", want: "not a sample\n"},
	{name: "unterminated label set", line: "metric{unterminated=\"value} 1\n", want: "metric{unterminated=\"value} 1\n"},
}

func writeLineOutput(line, extraLabels []byte) []byte {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	_ = writeLine(w, line, extraLabels)
	_ = w.Flush()
	return buf.Bytes()
}

func Test_writeLine(t *testing.T) {
	for _, tt := range writeLineTests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(writeLineOutput([]byte(tt.line), []byte(`foo="bar"`)))
			if got != tt.want {
				t.Errorf("want %q, got %q", tt.want, got)
			}
		})
	}
}

func Fuzz_writeLine(f *testing.F) {
	for _, tt := range writeLineTests {
		f.Add(tt.line)
	}
	f.Add("\x00\xff\n")
	f.Add(strings.Repeat("x", 4*1024))

	extraLabels := []byte(`fuzz_label="value"`)
	allowedInsertions := [][]byte{
		extraLabels,
		append([]byte{','}, extraLabels...),
		fmt.Appendf(nil, "{%s}", extraLabels),
	}
	f.Fuzz(func(t *testing.T, line string) {
		input := []byte(line)
		original := bytes.Clone(input)
		got := writeLineOutput(input, extraLabels)

		if !bytes.Equal(input, original) {
			t.Fatal("writeLine modified its input")
		}
		validOutput := bytes.Equal(got, original)
		for _, inserted := range allowedInsertions {
			validOutput = validOutput || isSingleInsertion(original, got, inserted)
		}
		if !validOutput {
			t.Fatalf("output contains an unexpected change: input=%q output=%q", original, got)
		}
	})
}

func Test_writeLineValidSamples(t *testing.T) {
	const name = "metric_name"
	extraLabels := []byte(`extra_label="value"`)
	labelSets := []struct {
		input string
		want  string
	}{
		{want: `{extra_label="value"}`},
		{input: `{existing="value"}`, want: `{existing="value",extra_label="value"}`},
		{input: `{}`, want: `{extra_label="value"}`},
		{input: `{existing="}"}`, want: `{existing="}",extra_label="value"}`},
	}
	for _, labels := range labelSets {
		for _, separator := range []string{" ", "\t", "  "} {
			for _, value := range []string{"1", "-1.5e-3", "NaN", "+Inf", "-Inf"} {
				for _, timestamp := range []string{"", " 123"} {
					for _, newline := range []string{"", "\n"} {
						line := name + labels.input + separator + value + timestamp + newline
						want := name + labels.want + separator + value + timestamp + newline
						if got := string(writeLineOutput([]byte(line), extraLabels)); got != want {
							t.Fatalf("want %q, got %q", want, got)
						}
					}
				}
			}
		}
	}
}

func isSingleInsertion(original, got, inserted []byte) bool {
	if len(got) != len(original)+len(inserted) {
		return false
	}
	for at := range len(original) + 1 {
		if bytes.Equal(got[:at], original[:at]) &&
			bytes.Equal(got[at:at+len(inserted)], inserted) &&
			bytes.Equal(got[at+len(inserted):], original[at:]) {
			return true
		}
	}
	return false
}
