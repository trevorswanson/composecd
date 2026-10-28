package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/hosts"
)

func TestSubscriptionsFanoutAndCancel(t *testing.T) {
	c := &Controller{}
	a, stopA := c.Subscribe()
	b, stopB := c.Subscribe()
	defer stopB()
	c.publish("app", "web")
	for _, ch := range []<-chan Update{a, b} {
		select {
		case u := <-ch:
			if u.Kind != "app" || u.Name != "web" {
				t.Fatal(u)
			}
		case <-time.After(time.Second):
			t.Fatal("no fanout")
		}
	}
	// A slow listener cannot stall the controller, and cancellation is idempotent.
	for i := 0; i < 100; i++ {
		c.publish("host", "nas")
	}
	stopA()
	stopA()
	<-a
	if _, ok := <-a; ok {
		t.Fatal("cancel did not close channel")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, cancel := c.Subscribe(); c.publish("operation", "web"); cancel() }()
	}
	wg.Wait()
}

func TestStatusAndOperationPublish(t *testing.T) {
	e := newEnv(t)
	e.c.RefreshAll(context.Background())
	changes, cancel := e.c.Subscribe()
	defer cancel()
	st := e.app("web")
	e.c.setStatus(&st)
	if u := <-changes; u.Kind != "app" {
		t.Fatal(u)
	}
	if err := e.c.Pause("web", "test"); err != nil {
		t.Fatal(err)
	}
	if u := <-changes; u.Kind != "app" {
		t.Fatal(u)
	}
	if err := e.c.SetMaintenance("nas", true, "test"); err != nil {
		t.Fatal(err)
	}
	if u := <-changes; u.Kind != "host" {
		t.Fatal(u)
	}
	op, err := e.c.startOperation("web", "sync", "manual", "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if u := <-changes; u.Kind != "operation" {
		t.Fatal(u)
	}
	op.Phase = "Succeeded"
	e.c.finishOperation("web", op)
	if u := <-changes; u.Kind != "operation" {
		t.Fatal(u)
	}
}

type logBackend struct {
	hosts.Backend
	called bool
}

func (b *logBackend) Logs(_ context.Context, _ *hosts.Host, container string, tail int, follow bool, w io.Writer) error {
	b.called = true
	if container != "web-web-1" || tail != 200 || !follow {
		return errors.New("wrong log arguments")
	}
	_, err := io.WriteString(w, "logs")
	return err
}
func TestLogsVerifyFreshProjectMembership(t *testing.T) {
	e := newEnv(t)
	b := &logBackend{Backend: e.backend}
	e.c.Hosts.Backend = b
	e.c.RefreshAll(context.Background())
	e.fake().Containers = map[string][]compose.Container{"web": {
		{Name: "web-web-1", Labels: compose.Labels{"com.docker.compose.project": "web"}},
		{Name: "foreign-web-1", Labels: compose.Labels{"com.docker.compose.project": "foreign"}},
		{Name: "no-label"},
	}}
	for _, name := range []string{"foreign-web-1", "no-label", "missing", "-option"} {
		err := e.c.Logs(context.Background(), "web", name, 200, true, io.Discard)
		if !errors.Is(err, ErrNotFound) || b.called {
			t.Fatalf("%s: %v called=%v", name, err, b.called)
		}
	}
	var out bytes.Buffer
	if err := e.c.Logs(context.Background(), "web", "web-web-1", 200, true, &out); err != nil || out.String() != "logs" {
		t.Fatalf("%v: %s", err, &out)
	}
	// A container that used to belong to the app is checked again on every call.
	b.called = false
	e.fake().Containers["web"] = nil
	if err := e.c.Logs(context.Background(), "web", "web-web-1", 200, true, io.Discard); !errors.Is(err, ErrNotFound) || b.called {
		t.Fatal(err)
	}
}

// Plain values are in Git already; masking them would make every diff and log
// unreadable (a "true" or "8080" anywhere). Only decrypted SOPS values count.
func TestMaskLeavesPlainValues(t *testing.T) {
	e := newEnv(t)
	e.c.RefreshAll(context.Background())
	e.c.mu.Lock()
	app := e.c.objects.Application("web")
	app.Spec.Source.Values = map[string]interface{}{"enabled": "true", "port": "8080", "image": "nginx:1.27"}
	app.Spec.Source.Env = map[string]string{"TZ": "Europe/London"}
	e.c.mu.Unlock()
	text := "enabled: true, port 8080, nginx:1.27, Europe/London"
	if got := e.c.Mask("web", text); got != text {
		t.Fatal(got)
	}
	e.c.remember("web", []string{"long-password"})
	if got := e.c.Mask("web", "pw long-password on 8080"); got != "pw •••••• on 8080" {
		t.Fatal(got)
	}
}
