package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db", "composecd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("database mode = %v, %v", fi.Mode(), err)
		}
	}

	op := &Operation{App: "media", Kind: KindSync, Trigger: TriggerAuto, SourceRevision: "abc"}
	if err := s.StartOperation(op); err != nil {
		t.Fatal(err)
	}
	if op.ID == 0 || op.Phase != PhaseRunning || op.StartedAt.IsZero() {
		t.Fatalf("started = %+v", op)
	}
	op.Phase, op.Message, op.Log, op.ReleaseRevision = PhaseSucceeded, "Upgrade complete", "pulling\nstarted\n", 4
	if err := s.FinishOperation(op); err != nil {
		t.Fatal(err)
	}
	second := &Operation{App: "media", Kind: KindRollback, Trigger: TriggerManual}
	if err := s.StartOperation(second); err != nil {
		t.Fatal(err)
	}
	other := &Operation{App: "tools", Kind: KindSync, Trigger: TriggerManual}
	if err := s.StartOperation(other); err != nil {
		t.Fatal(err)
	}

	got, err := s.Operation(op.ID)
	if err != nil || got.Log != "pulling\nstarted\n" || got.ReleaseRevision != 4 || got.FinishedAt == nil || got.Phase != PhaseSucceeded {
		t.Errorf("operation = %+v, %v", got, err)
	}
	ops, err := s.Operations("media", 0)
	if err != nil || len(ops) != 2 || ops[0].ID != second.ID || ops[1].Log != "" {
		t.Errorf("operations = %+v, %v", ops, err)
	}
	if all, _ := s.Operations("", 10); len(all) != 3 {
		t.Errorf("all operations = %d", len(all))
	}
	if last, err := s.LastOperation("tools"); err != nil || last.ID != other.ID {
		t.Errorf("last = %+v, %v", last, err)
	}
	if last, err := s.LastOperation("nope"); err != nil || last != nil {
		t.Errorf("last of unknown app = %+v, %v", last, err)
	}
	if _, err := s.Operation(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing operation: %v", err)
	}

	// Reopening fails operations a crash left running.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ = s.Operation(second.ID)
	if got.Phase != PhaseFailed || got.FinishedAt == nil {
		t.Errorf("interrupted operation = %+v", got)
	}
	if got, _ = s.Operation(op.ID); got.Phase != PhaseSucceeded {
		t.Errorf("finished operation changed: %+v", got)
	}
}

func TestAppState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, err := s.AppState("media"); err != nil || st.Paused {
		t.Errorf("default = %+v, %v", st, err)
	}
	if err := s.SetPaused("media", true, "rolled back"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.AppState("media"); !st.Paused || st.PausedReason != "rolled back" {
		t.Errorf("paused = %+v", st)
	}
	if err := s.SetPaused("media", false, "ignored"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.AppState("media"); st.Paused || st.PausedReason != "" {
		t.Errorf("resumed = %+v", st)
	}
}

func TestHostState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, err := s.HostState("nas"); err != nil || st.Maintenance {
		t.Errorf("default = %+v, %v", st, err)
	}
	if err := s.SetMaintenance("nas", true, "replacing disks"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.HostState("nas"); !st.Maintenance || st.MaintenanceReason != "replacing disks" {
		t.Errorf("on = %+v", st)
	}
	if err := s.SetMaintenance("nas", false, "x"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.HostState("nas"); st.Maintenance || st.MaintenanceReason != "" {
		t.Errorf("off = %+v", st)
	}
}
