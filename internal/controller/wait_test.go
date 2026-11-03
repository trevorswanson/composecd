package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestRefreshWaitWaitsForALaterRefresh(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); e.c.Run(ctx) }()
	defer func() { cancel(); <-stopped }()

	wait, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	root, err := e.c.RefreshWait(wait)
	if err != nil || root.RefreshedAt.IsZero() || root.Revision == "" {
		t.Fatalf("RefreshWait = %+v, %v", root, err)
	}
	// A second call waits for a refresh that started after it.
	e.c.mu.RLock()
	before := e.c.refreshesStarted
	e.c.mu.RUnlock()
	if _, err := e.c.RefreshWait(wait); err != nil {
		t.Fatal(err)
	}
	e.c.mu.RLock()
	fetched := e.c.refreshesFetched
	e.c.mu.RUnlock()
	if fetched <= before {
		t.Errorf("answered by refresh %d, which started before the call (%d)", fetched, before)
	}

	gone, stop := context.WithCancel(ctx)
	stop()
	if _, err := e.c.RefreshWait(gone); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled RefreshWait = %v", err)
	}
}

func TestRefreshAppWait(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	if err := e.c.RefreshAppWait(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.RefreshAppWait(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown app: %v", err)
	}
}

func TestCheckImagesWaitReportsWhatChanged(t *testing.T) {
	e, reg := newImageEnv(t, true)
	e.refresh()
	check := func() ImageCheck {
		t.Helper()
		got, err := e.c.CheckImagesWait(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		e.c.ops.Wait()
		return got
	}

	if got := check(); got.Checked == 0 || len(got.Updated) != 0 || len(got.Failed) != 0 {
		t.Errorf("unchanged check = %+v", got)
	}
	reg.set(nginx1, digestB)
	if got := check(); !slices.Equal(got.Updated, []string{nginx1}) || len(got.Failed) != 0 {
		t.Errorf("moved tag check = %+v", got)
	}
	reg.setDown(true)
	if got := check(); len(got.Updated) != 0 || got.Failed[nginx1] == "" {
		t.Errorf("outage check = %+v", got)
	}
	if _, err := e.c.CheckImagesWait(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown app: %v", err)
	}
}
