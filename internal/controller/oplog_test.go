package controller

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composecd/internal/store"
)

func TestFollowOperation(t *testing.T) {
	e := newEnv(t)
	c := e.c
	op, err := c.startOperation("web", store.KindSync, store.TriggerManual, "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if st := c.withRunning(AppStatus{Name: "web"}); st.Operation == nil || st.Operation.Phase != store.PhaseRunning {
		t.Fatalf("running operation not shown: %+v", st.Operation)
	}
	b := c.opLog(op)
	_, _ = b.Write([]byte("pulling\nstart"))
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- c.FollowOperation(context.Background(), op.ID, &out) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "pulling\n") {
		if time.Now().After(deadline) {
			t.Fatalf("no output while running: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(out.String(), "start") {
		t.Fatalf("partial line written early: %q", out.String())
	}
	_, _ = b.Write([]byte("ed web\n"))
	op.Phase, op.Log = store.PhaseSucceeded, b.String()
	c.finishOperation("web", op)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow didn't end with the operation")
	}
	if got := out.String(); got != "pulling\nstarted web\n" {
		t.Fatalf("got %q", got)
	}
	// A finished operation replays its stored log.
	var again bytes.Buffer
	if err := c.FollowOperation(context.Background(), op.ID, &again); err != nil || again.String() != "pulling\nstarted web\n" {
		t.Fatalf("%v %q", err, again.String())
	}
	if err := c.FollowOperation(context.Background(), 999, &again); err == nil {
		t.Fatal("unknown operation")
	}
}

// syncBuffer is a bytes.Buffer safe to read while another goroutine writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
