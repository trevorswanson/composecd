package web

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composecd/internal/auth"
)

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// frame prefixes every line, so output cannot inject SSE fields or events.
func frame(w http.ResponseWriter, event, data string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.ReplaceAll(data, "\r", ""), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return rc.Flush()
}

func (s *Server) Events(w http.ResponseWriter, r *http.Request) {
	p := s.identify(w, r, auth.RoleReadonly)
	if p == nil {
		return
	}
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "/apps"
	}
	if page != "/" && page != "/apps" && page != "/hosts" && page != "/windows" && page != "/operations" && page != "/applicationsets" && !strings.HasPrefix(page, "/apps/") && !strings.HasPrefix(page, "/hosts/") {
		http.Error(w, "Invalid event page", 400)
		return
	}
	view := r.Clone(context.WithValue(r.Context(), liveKey{}, true))
	view.URL.Path = page
	if strings.HasPrefix(page, "/apps/") || strings.HasPrefix(page, "/hosts/") {
		name, err := urlPathName(page)
		if err != nil {
			pageError(w, err)
			return
		}
		view.SetPathValue("name", name)
	}
	// Host pages also refresh when a background discovery scan starts or finishes.
	var scans <-chan struct{}
	if strings.HasPrefix(page, "/hosts") {
		scans = s.scans()
	}
	changes, cancel := s.C.Subscribe()
	defer cancel()
	if _, err := s.data(view, p); err != nil {
		pageError(w, err)
		return
	}
	streamHeaders(w)
	// Subscribe before the first snapshot so reconnects and changes during page
	// navigation cannot leave the browser showing stale data.
	send := func(kind string) error {
		current, err := s.Auth.Identify(r)
		if err != nil || current.Role != p.Role {
			return auth.ErrUnauthenticated
		}
		d, err := s.data(view, current)
		if err != nil {
			return err
		}
		d.OOB = true
		var b bytes.Buffer
		if err = templates.ExecuteTemplate(&b, "panel", d); err != nil {
			return err
		}
		return frame(w, kind, b.String())
	}
	if err := send("app"); err != nil {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case change, ok := <-changes:
			if !ok {
				return
			}
			if err := send(change.Kind); err != nil {
				return
			}
		case <-scans:
			scans = s.scans()
			if err := send("host"); err != nil {
				return
			}
		case <-tick.C:
			// Window state changes with the clock even without a reconcile.
			if err := send("app"); err != nil {
				return
			}
		}
	}
}

func urlPathName(page string) (string, error) {
	_, name, _ := strings.Cut(strings.TrimPrefix(page, "/"), "/")
	if name == "" || strings.Contains(name, "/") {
		return "", invalidRequest("invalid page path")
	}
	return name, nil
}

type logStream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	mask    func(string) string
	pending []byte
	started bool
	err     error
}

// Start is called only after the controller verifies project membership.
func (l *logStream) Start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.startLocked()
}

func (l *logStream) startLocked() {
	if l.started {
		return
	}
	l.started = true
	streamHeaders(l.w)
	l.err = frame(l.w, "ready", "")
}

func (l *logStream) emit(line string) {
	if l.err == nil {
		l.err = frame(l.w, "log", "<span>"+html.EscapeString(l.mask(line))+"</span>")
	}
}

func (l *logStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.startLocked()
	l.pending = append(l.pending, p...)
	for {
		i := bytes.IndexByte(l.pending, '\n')
		if i < 0 {
			break
		}
		l.emit(string(l.pending[:i+1]))
		l.pending = l.pending[i+1:]
	}
	if len(l.pending) > 1<<20 {
		return 0, fmt.Errorf("log line exceeds 1 MiB")
	}
	return len(p), l.err
}

func (s *Server) Logs(w http.ResponseWriter, r *http.Request) {
	p := s.identify(w, r, auth.RoleReadonly)
	if p == nil {
		return
	}
	tail := 200
	if value := r.URL.Query().Get("tail"); value != "" {
		n, err := parseTail(value)
		if err != nil {
			pageError(w, err)
			return
		}
		tail = n
	}
	follow := r.URL.Query().Get("follow") == "1"
	stream := &logStream{w: w, mask: func(text string) string { return s.C.Mask(r.PathValue("name"), text) }}
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	// Revocation and session expiry also end an otherwise idle log follow.
	go func() {
		defer close(done)
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				current, err := s.Auth.Identify(r)
				if err != nil || current.Role != p.Role {
					cancel()
					return
				}
				stream.mu.Lock()
				if stream.started && stream.err == nil {
					stream.err = frame(w, "ping", "")
				}
				failed := stream.err != nil
				stream.mu.Unlock()
				if failed {
					cancel()
					return
				}
			}
		}
	}()
	err := s.C.Logs(ctx, r.PathValue("name"), r.PathValue("container"), tail, follow, stream)
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if !stream.started && err != nil {
		pageError(w, err)
		return
	}
	stream.startLocked()
	if len(stream.pending) > 0 {
		stream.emit(string(stream.pending))
	}
	if err != nil && ctx.Err() == nil {
		_ = frame(w, "error", html.EscapeString(s.C.Mask(r.PathValue("name"), err.Error())))
	}
	_ = frame(w, "end", "")
}

func parseTail(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 10000 {
		return 0, invalidRequest("tail must be between 0 and 10000")
	}
	return n, nil
}
