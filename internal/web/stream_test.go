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
