package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/transform"
)

func Test(t *testing.T) {
	t.Log("this test depends on node_exporter on localhost:9100")

	cfg, err := loadConfig("./testdata/config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h := handler((*cfg)["default"].Exporters)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.ServeHTTP(rec, req)

	nodeExpResp, err := http.Get("http://localhost:9100/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer nodeExpResp.Body.Close()

	want := countLines(t, nodeExpResp.Body) * 2 // proxy to node_exporter twice in config.yaml
	got := countLines(t, rec.Body)
	if want != got {
		t.Errorf("line count should be same as upstream exporters: want=%d, got=%d", want, got)
	}
}

func countLines(t *testing.T, r io.Reader) int {
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) != 0 {
			n++
		}
	}
	return n
}

func Test_transformer(t *testing.T) {
	origin := strings.Join([]string{
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
	want := strings.Join([]string{
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
	tr := transform.NewReader(strings.NewReader(origin), transformer([]string{`foo="bar"`}))
	b, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.EqualFold(want, got) {
		t.Error("unexpected result \n" + got)
	}
}
