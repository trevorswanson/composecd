package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
)

type request struct {
	path    string
	headers http.Header
	body    string
}

func TestDispatch(t *testing.T) {
	var mu sync.Mutex
	var got []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, request{r.URL.Path, r.Header.Clone(), string(b)})
		mu.Unlock()
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	urlFile := filepath.Join(dir, "url")
	_ = os.WriteFile(tokenFile, []byte("tok\n"), 0o600)
	_ = os.WriteFile(urlFile, []byte(srv.URL+"/discord\n"), 0o600)
	d, err := New([]config.Notifier{
		{Name: "phone", Type: "ntfy", URL: srv.URL + "/ntfy", TokenFile: tokenFile},
		{Name: "gotify", Type: "gotify", URL: srv.URL + "/gotify/", TokenFile: tokenFile, Events: []string{config.EventSyncSucceeded}},
		{Name: "chat", Type: "discord", URLFile: urlFile, Applications: []string{"media*"}},
		{Name: "slack", Type: "slack", URL: srv.URL + "/slack", Events: []string{config.EventHostDown}},
		{Name: "hook", Type: "webhook", URL: srv.URL + "/hook", Events: config.AllEvents},
		{Name: "broken", Type: "webhook", URL: srv.URL + "/broken"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	when := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	d.Notify(controller.Event{Type: config.EventSyncFailed, App: "media", Host: "nas", Message: "sync of media failed: boom", Time: when})
	d.Notify(controller.Event{Type: config.EventSyncSucceeded, App: "photos", Message: "ok", Time: when})
	d.Notify(controller.Event{Type: config.EventHostDown, Host: "pi", Message: "pi is down", Time: when})

	// media failed: ntfy, discord, hook, broken. photos ok: gotify, hook.
	// pi down: ntfy, discord (host events skip the app filter), slack, hook, broken.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 11 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	byPath := map[string][]request{}
	for _, r := range got {
		byPath[r.path] = append(byPath[r.path], r)
	}
	counts := map[string]int{"/ntfy": 2, "/gotify/message": 1, "/discord": 2, "/slack": 1, "/hook": 3, "/broken": 2}
	for p, n := range counts {
		if len(byPath[p]) != n {
			t.Errorf("%s: %d requests, want %d", p, len(byPath[p]), n)
		}
	}
	if len(got) != 11 {
		t.Fatalf("got %d requests: %+v", len(got), got)
	}

	ntfy := byPath["/ntfy"][0]
	if ntfy.body != "sync of media failed: boom" || ntfy.headers.Get("Title") != "composecd: media: sync failed" ||
		ntfy.headers.Get("Priority") != "high" || ntfy.headers.Get("Authorization") != "Bearer tok" {
		t.Errorf("ntfy = %+v", ntfy)
	}
	if g := byPath["/gotify/message"][0]; g.headers.Get("X-Gotify-Key") != "tok" || !strings.Contains(g.body, `"priority":4`) {
		t.Errorf("gotify = %+v", g)
	}
	if dc := byPath["/discord"][0]; !strings.Contains(dc.body, `**composecd: media: sync failed**\nsync of media failed: boom`) {
		t.Errorf("discord = %s", dc.body)
	}
	if sl := byPath["/slack"][0]; !strings.Contains(sl.body, `*composecd: pi: host down*`) {
		t.Errorf("slack = %s", sl.body)
	}
	var hook map[string]interface{}
	if err := json.Unmarshal([]byte(byPath["/hook"][0].body), &hook); err != nil || hook["type"] != "sync-failed" ||
		hook["app"] != "media" || hook["title"] != "media: sync failed" {
		t.Errorf("webhook = %v (%v)", hook, err)
	}
}

func TestNewErrors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New([]config.Notifier{{Name: "a", Type: "ntfy", URLFile: "/nonexistent/url"}}, log); err == nil {
		t.Error("missing urlFile accepted")
	}
	if _, err := New([]config.Notifier{{Name: "a", Type: "ntfy", URL: "u", TokenFile: "/nonexistent/t"}}, log); err == nil {
		t.Error("missing tokenFile accepted")
	}
	d, _ := New(nil, log)
	d.Notify(controller.Event{Type: config.EventSyncFailed}) // no notifiers: dropped silently
}
