package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

type fakeHousekeeper struct {
	mu       sync.Mutex
	usage    hosts.DiskUsage
	err      error
	measured int
	prunes   []string
}

func (f *fakeHousekeeper) DiskUsage(_ context.Context, h *hosts.Host) (*hosts.DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.measured++
	if f.err != nil {
		return nil, f.err
	}
	u := f.usage
	return &u, nil
}

func (f *fakeHousekeeper) PruneImages(_ context.Context, h *hosts.Host, o hosts.PruneOptions, w io.Writer) error {
	f.record(fmt.Sprintf("image-prune %s all=%v until=%v", h.Name(), o.All, o.Until))
	fmt.Fprintln(w, "Deleted Images:\nuntagged: nginx:1\n\nTotal reclaimed space: 1.2GB")
	return nil
}

func (f *fakeHousekeeper) PruneBuildCache(_ context.Context, h *hosts.Host, o hosts.PruneOptions, w io.Writer) error {
	f.record(fmt.Sprintf("builder-prune %s all=%v keep=%s", h.Name(), o.All, o.KeepStorage))
	fmt.Fprintln(w, "ID\tRECLAIMABLE\tSIZE\nabc\ttrue\t1MB\nTotal:\t3.4GB")
	return nil
}

func (f *fakeHousekeeper) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes = append(f.prunes, s)
}

func (f *fakeHousekeeper) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.prunes, "; ")
}

func (f *fakeHousekeeper) set(used, avail int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = hosts.DiskUsage{Root: "/var/lib/docker", Size: used + avail, Used: used, Available: avail,
		Kinds: []hosts.DiskKind{{Kind: "images", Count: 3, Size: 3e9, Reclaimable: 2e9}}}
}

const housekeepingHost = `apiVersion: composecd/v1alpha1
kind: Host
metadata: {name: nas}
spec:
  projectRoot: /srv
  housekeeping:
    imagePrune: {schedule: "0 4 * * 0", timeZone: Europe/Berlin, until: 168h, all: true}
    builderPrune: {schedule: "0 4 * * 0", keepStorage: 10GB}
    diskWarning: {percent: 85}
`

// downRunner is an engine that doesn't answer.
type downRunner struct{ compose.Runner }

func (downRunner) Version(context.Context) (string, error) {
	return "", errors.New("connection refused")
}

func newHousekeepingEnv(t *testing.T) (*env, *fakeHousekeeper, *recorder) {
	t.Helper()
	e := newEnv(t)
	e.commit(map[string]string{"composecd/hosts.yaml": housekeepingHost, "composecd/apps.yaml": appYAML("manual", false, "")})
	hk := &fakeHousekeeper{}
	hk.set(50, 50)
	rec := &recorder{}
	e.c.Housekeeper, e.c.Notify = hk, rec
	return e, hk, rec
}

func TestPruneClock(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	job := func(host, schedule string) pruneJob {
		h := &hosts.Host{Spec: &config.Host{Metadata: config.Metadata{Name: host}, Spec: config.HostSpec{Housekeeping: &config.Housekeeping{
			ImagePrune: &config.ImagePrune{Schedule: schedule, TimeZone: "Europe/Berlin"}}}}}
		return pruneJobs([]*hosts.Host{h})[0]
	}
	weekly := job("nas", "0 4 * * 0")
	sat := time.Date(2026, 11, 7, 12, 0, 0, 0, berlin)
	sun4 := time.Date(2026, 11, 8, 4, 0, 0, 0, berlin)

	var clock pruneClock
	due, wake := clock.due(sat, []pruneJob{weekly})
	if len(due) != 0 || !wake.Equal(sun4) {
		t.Fatalf("first look: due %v, wake %v", due, wake)
	}
	if due, _ := clock.due(sun4.Add(-time.Second), []pruneJob{weekly}); len(due) != 0 {
		t.Errorf("due a second early: %v", due)
	}
	due, wake = clock.due(sun4.Add(30*time.Second), []pruneJob{weekly})
	if len(due) != 1 || due[0].host != "nas" || due[0].kind != store.KindImagePrune || !wake.Equal(sun4.AddDate(0, 0, 7)) {
		t.Errorf("at 4:00: due %v, wake %v", due, wake)
	}
	if due, _ := clock.due(sun4.Add(time.Minute), []pruneJob{weekly}); len(due) != 0 {
		t.Errorf("due twice: %v", due)
	}

	// A changed schedule starts over, rather than firing for the old one.
	daily := job("nas", "0 3 * * *")
	due, wake = clock.due(sun4.AddDate(0, 0, 7), []pruneJob{daily})
	if len(due) != 0 || !wake.Equal(time.Date(2026, 11, 16, 3, 0, 0, 0, berlin)) || len(clock.next) != 1 {
		t.Errorf("after a change: due %v, wake %v, %d tracked", due, wake, len(clock.next))
	}
	if _, wake := clock.due(sat, nil); !wake.IsZero() || len(clock.next) != 0 {
		t.Errorf("no jobs: wake %v, %d tracked", wake, len(clock.next))
	}

	// Each host's schedules, unset ones left out.
	h := &hosts.Host{Spec: &config.Host{Metadata: config.Metadata{Name: "pi"}, Spec: config.HostSpec{Housekeeping: &config.Housekeeping{
		ImagePrune: &config.ImagePrune{}, BuilderPrune: &config.BuilderPrune{Schedule: "@daily"}}}}}
	if jobs := pruneJobs([]*hosts.Host{h}); len(jobs) != 1 || jobs[0].kind != store.KindBuilderPrune || jobs[0].loc != time.UTC {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestScheduledPruneSkipsMaintenanceAndDownHosts(t *testing.T) {
	e, hk, _ := newHousekeepingEnv(t)
	e.refresh()
	ctx := context.Background()
	kinds := []string{store.KindImagePrune, store.KindBuilderPrune}

	if err := e.c.SetMaintenance("nas", true, "disk swap"); err != nil {
		t.Fatal(err)
	}
	e.c.scheduledPrune(ctx, "nas", kinds)
	if err := e.c.SetMaintenance("nas", false, ""); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()

	e.backend.mu.Lock()
	e.backend.wrap = func(r compose.Runner) compose.Runner { return downRunner{r} }
	e.backend.mu.Unlock()
	e.refresh()
	if s := e.c.Hosts.Get("nas").Status(); s.State != hosts.StateDown {
		t.Fatalf("state = %s", s.State)
	}
	e.c.scheduledPrune(ctx, "nas", kinds)
	if hk.calls() != "" {
		t.Fatalf("pruned a host in maintenance or down: %s", hk.calls())
	}
	if ops, _ := e.c.HostOperations("nas", 0); len(ops) != 0 {
		t.Errorf("skipped prunes recorded: %+v", ops)
	}

	e.backend.mu.Lock()
	e.backend.wrap = nil
	e.backend.mu.Unlock()
	e.refresh()
	e.c.scheduledPrune(ctx, "nas", kinds)
	if got := hk.calls(); got != "builder-prune nas all=false keep=10GB; image-prune nas all=true until=168h0m0s" {
		t.Errorf("prunes = %s", got)
	}
	ops, err := e.c.HostOperations("nas", 0)
	if err != nil || len(ops) != 2 {
		t.Fatalf("operations = %+v, %v", ops, err)
	}
	if op := ops[0]; op.Kind != store.KindImagePrune || op.Host != "nas" || op.App != "" || op.Trigger != store.TriggerAuto ||
		op.Phase != store.PhaseSucceeded || op.Message != "reclaimed 1.2GB" {
		t.Errorf("image prune = %+v", op)
	}
	if op := ops[1]; op.Kind != store.KindBuilderPrune || op.Message != "reclaimed 3.4GB" {
		t.Errorf("builder prune = %+v", op)
	}
	if full, err := e.c.Operation(ops[0].ID); err != nil || !strings.Contains(full.Log, "untagged: nginx:1") {
		t.Errorf("log = %+v, %v", full, err)
	}
	if ops, err := e.c.Operations("manual", 0); err != nil || len(ops) != 0 {
		t.Errorf("app operations = %+v, %v", ops, err)
	}
}

func TestPruneWaitsForOperations(t *testing.T) {
	wait, poll := pruneWait, prunePoll
	pruneWait, prunePoll = 100*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { pruneWait, prunePoll = wait, poll })
	e, hk, _ := newHousekeepingEnv(t)
	e.refresh()
	h := e.c.Hosts.Get("nas")

	// An operation that outlasts pruneWait: the prune is skipped.
	done := h.Use()
	op, err := e.c.PruneHost("nas", store.KindImagePrune, false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.PruneHost("nas", store.KindBuilderPrune, false, "admin"); !errors.Is(err, ErrHostBusy) {
		t.Errorf("second prune: %v", err)
	}
	e.c.Wait()
	done()
	got, _ := e.c.Operation(op.ID)
	if got.Phase != store.PhaseFailed || !strings.HasPrefix(got.Message, "skipped: operations on nas were still running") || hk.calls() != "" {
		t.Errorf("busy host: %+v, prunes %q", got, hk.calls())
	}

	// One that ends in time: the prune runs after it, and never alongside.
	pruneWait = 10 * time.Second
	done = h.Use()
	op, err = e.c.PruneHost("nas", store.KindImagePrune, true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if hk.calls() != "" {
		t.Fatal("pruned while an operation was using the host")
	}
	done()
	e.c.Wait()
	got, _ = e.c.Operation(op.ID)
	if got.Phase != store.PhaseSucceeded || got.User != "admin" || got.Trigger != store.TriggerManual ||
		hk.calls() != "image-prune nas all=true until=168h0m0s" {
		t.Errorf("after the operation: %+v, prunes %q", got, hk.calls())
	}
}

func TestManualPrune(t *testing.T) {
	e, hk, _ := newHousekeepingEnv(t)
	e.refresh()
	// Manual operations work in maintenance.
	if err := e.c.SetMaintenance("nas", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.PruneHost("nas", store.KindBuilderPrune, true, "admin"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if got := hk.calls(); got != "builder-prune nas all=true keep=10GB" {
		t.Errorf("prunes = %s", got)
	}
	if _, err := e.c.PruneHost("nope", store.KindImagePrune, false, "admin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
	if _, err := e.c.PruneHost("nas", "volume-prune", false, "admin"); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, err := e.c.HostOperations("nope", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host's operations: %v", err)
	}
	if m := e.c.OperationMetrics(); len(m) != 1 || m[0].Kind != store.KindBuilderPrune || m[0].Count != 1 {
		t.Errorf("metrics = %+v", m)
	}
	e.c.Housekeeper = nil
	if _, err := e.c.PruneHost("nas", store.KindImagePrune, false, "admin"); err == nil {
		t.Error("pruned without a housekeeper")
	}
}

func TestDiskWarningOncePerCrossing(t *testing.T) {
	interval := diskInterval
	t.Cleanup(func() { diskInterval = interval })
	e, hk, rec := newHousekeepingEnv(t)
	warnings := func() int { return strings.Count(rec.types(), config.EventHostDisk+":") }

	e.refresh()
	hs := e.c.HostStatuses()[0]
	if hs.Disk == nil || hs.Disk.Used != 50 || hs.DiskWarning != "" || warnings() != 0 {
		t.Fatalf("half full: %+v, %d warnings", hs, warnings())
	}
	if next := hs.NextPrune[store.KindImagePrune]; next.IsZero() || next.Weekday() != time.Sunday {
		t.Errorf("next prune = %v", hs.NextPrune)
	}
	// Throttled: the next ping doesn't measure again.
	e.refresh()
	if hk.measured != 1 {
		t.Errorf("measured %d times", hk.measured)
	}

	diskInterval = 0
	hk.set(90, 10)
	e.refresh()
	hs = e.c.HostStatuses()[0]
	if hs.DiskWarning != "the disk is 90% full (warning at 85%)" || warnings() != 1 {
		t.Fatalf("over: %q, %d warnings", hs.DiskWarning, warnings())
	}
	if ev := rec.events[len(rec.events)-1]; ev.Host != "nas" || !strings.Contains(ev.Message, "90% full") {
		t.Errorf("event = %+v", ev)
	}
	e.refresh()
	hk.err = errors.New("permission denied")
	e.refresh()
	if warnings() != 1 {
		t.Errorf("still over, or unmeasured: %d warnings", warnings())
	}
	if hs := e.c.HostStatuses()[0]; hs.Disk.Error != "permission denied" || hs.DiskWarning != "" {
		t.Errorf("failed measurement: %+v", hs.Disk)
	}
	hk.err = nil
	hk.set(80, 20)
	e.refresh()
	hk.set(95, 5)
	e.refresh()
	if warnings() != 2 {
		t.Errorf("after dropping below and crossing again: %d warnings", warnings())
	}

	// Hosts without housekeeping aren't measured.
	e.commit(map[string]string{"composecd/hosts.yaml": hostsYAML})
	before := hk.measured
	e.refresh()
	if hs := e.c.HostStatuses()[0]; hs.Disk != nil || hs.NextPrune != nil || hk.measured != before {
		t.Errorf("without housekeeping: %+v, measured %d", hs, hk.measured-before)
	}
}
