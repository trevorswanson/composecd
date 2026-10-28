// Package notify sends controller events to ntfy, Gotify, Discord, Slack or a
// generic JSON webhook. Sending is asynchronous: Notify never blocks the
// controller, and failures are logged.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
)

// sender is one configured notifier.
type sender struct {
	cfg    config.Notifier
	url    string
	token  string
	events map[string]bool
}

// Dispatcher queues events and sends them to every notifier that wants them.
type Dispatcher struct {
	senders []*sender
	queue   chan controller.Event
	client  *http.Client
	log     *slog.Logger
}

// New prepares the notifiers, reading their URL and token files.
func New(cfgs []config.Notifier, log *slog.Logger) (*Dispatcher, error) {
	d := &Dispatcher{queue: make(chan controller.Event, 256), client: &http.Client{Timeout: 15 * time.Second}, log: log}
	for _, c := range cfgs {
		s := &sender{cfg: c, url: c.URL, events: map[string]bool{}}
		if c.URLFile != "" {
			b, err := os.ReadFile(c.URLFile)
			if err != nil {
				return nil, fmt.Errorf("notification %q: reading urlFile: %w", c.Name, err)
			}
			s.url = strings.TrimSpace(string(b))
		}
		if c.TokenFile != "" {
			b, err := os.ReadFile(c.TokenFile)
			if err != nil {
				return nil, fmt.Errorf("notification %q: reading tokenFile: %w", c.Name, err)
			}
			s.token = strings.TrimSpace(string(b))
		}
		events := c.Events
		if len(events) == 0 {
			events = config.DefaultEvents
		}
		for _, e := range events {
			s.events[e] = true
		}
		d.senders = append(d.senders, s)
	}
	return d, nil
}

// Notify queues e. If the queue is full (a notifier is stuck), e is dropped.
func (d *Dispatcher) Notify(e controller.Event) {
	if len(d.senders) == 0 {
		return
	}
	select {
	case d.queue <- e:
	default:
		d.log.Warn("notification queue full; dropping an event", "type", e.Type, "app", e.App)
	}
}

// Run sends queued events until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-d.queue:
			for _, s := range d.senders {
				if !s.wants(e) {
					continue
				}
				if err := d.send(ctx, s, e); err != nil {
					d.log.Warn("sending a notification", "notifier", s.cfg.Name, "type", e.Type, "app", e.App, "error", err)
				}
			}
		}
	}
}

func (s *sender) wants(e controller.Event) bool {
	if !s.events[e.Type] {
		return false
	}
	if e.App == "" || len(s.cfg.Applications) == 0 {
		return true
	}
	for _, g := range s.cfg.Applications {
		if ok, _ := path.Match(g, e.App); ok {
			return true
		}
	}
	return false
}

// Title is a one-line summary of e, e.g. "media: sync failed".
func Title(e controller.Event) string {
	what := strings.ReplaceAll(e.Type, "-", " ")
	switch {
	case e.App != "":
		return e.App + ": " + what
	case e.Host != "":
		return e.Host + ": " + what
	}
	return what
}

func bad(e controller.Event) bool {
	switch e.Type {
	case config.EventSyncFailed, config.EventHealthDegraded, config.EventHostDown:
		return true
	}
	return false
}

func (d *Dispatcher) send(ctx context.Context, s *sender, e controller.Event) error {
	title, msg := Title(e), e.Message
	var (
		body    interface{}
		raw     []byte
		headers = map[string]string{}
		url     = s.url
	)
	switch s.cfg.Type {
	case "ntfy":
		raw = []byte(msg)
		headers["Title"] = "composecd: " + title
		headers["Tags"] = "white_check_mark"
		if bad(e) {
			headers["Tags"], headers["Priority"] = "warning", "high"
		}
		if s.token != "" {
			headers["Authorization"] = "Bearer " + s.token
		}
	case "gotify":
		url = strings.TrimRight(url, "/") + "/message"
		priority := 4
		if bad(e) {
			priority = 8
		}
		body = map[string]interface{}{"title": "composecd: " + title, "message": msg, "priority": priority}
		headers["X-Gotify-Key"] = s.token
	case "discord":
		body = map[string]string{"content": fmt.Sprintf("**composecd: %s**\n%s", title, msg)}
	case "slack":
		body = map[string]string{"text": fmt.Sprintf("*composecd: %s*\n%s", title, msg)}
	default: // webhook
		body = struct {
			controller.Event
			Title string `json:"title"`
		}{e, title}
	}
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
		headers["Content-Type"] = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
