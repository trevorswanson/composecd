package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFrameMultiline(t *testing.T) {
	w := httptest.NewRecorder()
	if err := frame(w, "app", "<div>\nline\r\ndata: not another event\n</div>"); err != nil {
		t.Fatal(err)
	}
	want := "event: app\ndata: <div>\ndata: line\ndata: data: not another event\ndata: </div>\n\n"
	if w.Body.String() != want {
		t.Fatalf("%q", w.Body.String())
	}
}

func TestLogStreamMasksAcrossWrites(t *testing.T) {
	w := httptest.NewRecorder()
	stream := &logStream{w: w, mask: func(s string) string { return strings.ReplaceAll(s, "secret-value", "••••••") }}
	for _, chunk := range []string{"<script>sec", "ret-va", "lue\nsecond\n"} {
		if _, err := stream.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	body := w.Body.String()
	if strings.Contains(body, "secret-value") || strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;••••••") || !strings.Contains(body, "event: ready") {
		t.Fatal(body)
	}
	if len(stream.pending) != 0 {
		t.Fatal("unflushed log line")
	}
}

func TestShortVersion(t *testing.T) {
	for in, want := range map[string]string{
		"dev":    "dev",
		"v1.4.0": "v1.4.0",
		"ci":     "ci",
		"dev-6e49ccac46c1d0f1b0a6e8d6f3c3a1c2b9e7d4f5": "dev-6e49cca",
		"v1.4.0-3-g6e49cca":                            "v1.4.0-3-g6e49cca",
		"dev-not-a-hash-but-long-enough":               "dev-not-a-hash-but-long-enough",
	} {
		if got := shortVersion(in); got != want {
			t.Errorf("shortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
