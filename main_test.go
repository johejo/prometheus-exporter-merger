package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "# HELP metric documentation\nmetric 1\n")
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

	if want, got := 4, countLines(t, strings.NewReader(body)); want != got {
		t.Errorf("line count should be same as upstream exporters: want=%d, got=%d", want, got)
	}
	if want, got := 2, strings.Count(body, `metric{foo="bar"} 1`); want != got {
		t.Errorf("all samples should contain common labels: want=%d, got=%d", want, got)
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
	longValue := strings.Repeat("x", 4*1024) // longer than bufio's default read buffer
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
		name   string
		origin string
		want   string
	}{
		{name: "exposition", origin: exposition, want: expositionWant},
		{
			name:   "line longer than the read buffer",
			origin: fmt.Sprintf("metric{value=%q} 1\n", longValue),
			want:   fmt.Sprintf("metric{value=%q,foo=\"bar\"} 1\n", longValue),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := bufio.NewWriter(&buf)
			if err := copyBody(w, io.NopCloser(strings.NewReader(tt.origin)), []byte(`foo="bar"`)); err != nil {
				t.Fatal(err)
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

// writeLineOutput collects what writeLine emits for line into a single slice, so
// tests can compare it as a value. bytes.Buffer never fails, so neither can the
// writes.
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

	t.Run("no extra labels", func(t *testing.T) {
		const line = "metric 1\n"
		if got := string(writeLineOutput([]byte(line), nil)); got != line {
			t.Errorf("want %q, got %q", line, got)
		}
	})
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
		// At most the labels themselves plus the "{}" or "," around them.
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
