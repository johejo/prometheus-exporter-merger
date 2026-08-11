package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

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
	handler(cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if want, got := 4, countLines(body); want != got {
		t.Errorf("duplicate metadata should be omitted: want=%d, got=%d", want, got)
	}
	if want, got := 1, strings.Count(body, "# HELP metric documentation"); want != got {
		t.Errorf("HELP should occur once: want=%d, got=%d", want, got)
	}
	if want, got := 1, strings.Count(body, "# TYPE metric gauge"); want != got {
		t.Errorf("TYPE should occur once: want=%d, got=%d", want, got)
	}
	if want, got := 2, strings.Count(body, `metric{foo="bar"} 1`); want != got {
		t.Errorf("all samples should contain common labels: want=%d, got=%d", want, got)
	}
}

func TestHandlerClosesAllResponseBodiesAfterCopyError(t *testing.T) {
	readErr := errors.New("read failed")
	bodies := map[string]*trackingReadCloser{
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

	originalHTTPClient := httpClient
	httpClient = func() *http.Client { return &http.Client{Transport: transport} }
	t.Cleanup(func() { httpClient = originalHTTPClient })

	cfg := ListenerConfig{
		Path: "/metrics",
		Exporters: map[string]Exporter{
			"first":  {URI: "http://first"},
			"second": {URI: "http://second"},
		},
	}
	handler(cfg).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

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

	originalHTTPClient := httpClient
	httpClient = func() *http.Client { return &http.Client{Transport: transport} }
	t.Cleanup(func() { httpClient = originalHTTPClient })

	cfg := ListenerConfig{
		Path: "/metrics",
		name: "header-check-test",
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
	handler(cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

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
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	originalHTTPClient := httpClient
	httpClient = func() *http.Client { return &http.Client{Transport: transport} }
	t.Cleanup(func() { httpClient = originalHTTPClient })

	cfg := ListenerConfig{
		Path: "/metrics",
		name: "timeout-test",
		Exporters: map[string]Exporter{
			"slow": {URI: "http://slow", Timeout: 10 * time.Millisecond},
		},
	}
	started := time.Now()
	rec := httptest.NewRecorder()
	handler(cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if got, want := rec.Code, http.StatusBadGateway; got != want {
		t.Errorf("status: got %d, want %d", got, want)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("upstream timeout took too long: %s", elapsed)
	}
}

func TestHandlerCancelsOtherUpstreamsAfterFailure(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "failure" {
			return nil, errors.New("connection failed")
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	originalHTTPClient := httpClient
	httpClient = func() *http.Client { return &http.Client{Transport: transport} }
	t.Cleanup(func() { httpClient = originalHTTPClient })

	cfg := ListenerConfig{
		Path: "/metrics",
		name: "cancel-test",
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
	handler(cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

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

func TestHandlerRecordsUpstreamDuration(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("metric 1\n")),
			Request:    req,
		}, nil
	})
	originalHTTPClient := httpClient
	httpClient = func() *http.Client { return &http.Client{Transport: transport} }
	t.Cleanup(func() { httpClient = originalHTTPClient })

	cfg := ListenerConfig{
		Path: "/metrics",
		name: "duration-test",
		Exporters: map[string]Exporter{
			"success": {URI: "http://success"},
		},
	}
	handler(cfg).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	var output bytes.Buffer
	metrics.WritePrometheus(&output, false)
	for _, want := range []string{
		`exporter_merger_upstream_requests_total{listener="duration-test",upstream="success"} 1`,
		`exporter_merger_upstream_request_duration_seconds_count{listener="duration-test",upstream="success"} 1`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("self metrics do not contain %q", want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingReadCloser struct {
	io.Reader
	closed atomic.Bool
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

func (r *trackingReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

func countLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if len(line) != 0 {
			n++
		}
	}
	return n
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

	line := []byte("metric{" + labels[0] + "} 1\n")
	if _, _, _, ok := labelInsertPoint(line); !ok {
		t.Errorf("generated label cannot be parsed: %q", line)
	}
}

func Test_copyBody(t *testing.T) {
	longValue := strings.Repeat("x", 2*readBufferSize)
	exposition := strings.Join([]string{
		`# HELP go_gc_duration_seconds A summary of the pause duration of garbage collection cycles.`,
		`# TYPE go_gc_duration_seconds summary`,
		`go_gc_duration_seconds{quantile="0"} 5.871e-06`,
		`go_gc_duration_seconds{quantile="0.25"} 8.356e-06`,
		`go_gc_duration_seconds{quantile="0.25"} 8.356e-06`,
		`go_gc_duration_seconds{quantile="0.5"} 1.2864e-05`,
		`go_gc_duration_seconds{quantile="0.75"} 1.8997e-05`,
		`go_gc_duration_seconds{quantile="1"} 5.5938e-05`,
		`go_gc_duration_seconds_sum 0.464658525`,
		`go_gc_duration_seconds_count 30719`,
	}, "\n")
	expositionWant := strings.Join([]string{
		`# HELP go_gc_duration_seconds A summary of the pause duration of garbage collection cycles.`,
		`# TYPE go_gc_duration_seconds summary`,
		`go_gc_duration_seconds{quantile="0",foo="bar"} 5.871e-06`,
		`go_gc_duration_seconds{quantile="0.25",foo="bar"} 8.356e-06`,
		`go_gc_duration_seconds{quantile="0.25",foo="bar"} 8.356e-06`,
		`go_gc_duration_seconds{quantile="0.5",foo="bar"} 1.2864e-05`,
		`go_gc_duration_seconds{quantile="0.75",foo="bar"} 1.8997e-05`,
		`go_gc_duration_seconds{quantile="1",foo="bar"} 5.5938e-05`,
		`go_gc_duration_seconds_sum{foo="bar"} 0.464658525`,
		`go_gc_duration_seconds_count{foo="bar"} 30719`,
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
	f.Fuzz(func(t *testing.T, line string) {
		input := []byte(line)
		original := bytes.Clone(input)
		got := writeLineOutput(input, extraLabels)

		if !bytes.Equal(input, original) {
			t.Fatal("writeLine modified its input")
		}
		if !isSubsequence(original, got) {
			t.Fatalf("output contains changes other than insertion: input=%q output=%q", original, got)
		}
		if delta := len(got) - len(original); delta < 0 || delta > len(extraLabels)+2 {
			t.Fatalf("unexpected output length change: input=%q output=%q", original, got)
		}
	})
}

func isSubsequence(want, got []byte) bool {
	for _, b := range got {
		if len(want) > 0 && b == want[0] {
			want = want[1:]
		}
	}
	return len(want) == 0
}
