package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

// Housekeeper measures a host's disk and prunes its images and build cache;
// hosts.CLI implements it. Without one, composecd does no housekeeping.
type Housekeeper interface {
	DiskUsage(ctx context.Context, h *hosts.Host) (*hosts.DiskUsage, error)
	PruneImages(ctx context.Context, h *hosts.Host, o hosts.PruneOptions, w io.Writer) error
	PruneBuildCache(ctx context.Context, h *hosts.Host, o hosts.PruneOptions, w io.Writer) error
}

// ErrHostBusy is a manual prune refused because another is running on the host.
var ErrHostBusy = errors.New("a housekeeping operation is already running on this host")

var (
	// diskInterval is how often host pings measure the disk.
	diskInterval = 10 * time.Minute
	diskTimeout  = 2 * time.Minute
	// pruneWait bounds how long a prune waits for the host's operations to
	// finish; a busy host's prune is skipped after it.
	pruneWait    = 15 * time.Minute
	prunePoll    = time.Second
	pruneTimeout = time.Hour
	// housekeepingTick is the longest the prune scheduler sleeps, so it sees
	// schedules added in Git.
	housekeepingTick = time.Minute
)

// diskState is a host's last disk measurement.
type diskState struct {
	usage     *hosts.DiskUsage
	measuring bool
	// warned is set while the disk is past its warning threshold, so the
	// host-disk event fires once per crossing.
	warned bool
}

// measureDisk measures h's disk in the background, if h has housekeeping and
// its last measurement is older than diskInterval (or force).
func (c *Controller) measureDisk(h *hosts.Host, force bool) {
	if c.Housekeeper == nil || h.Spec.Spec.Housekeeping == nil {
		return
	}
	name := h.Name()
	c.mu.Lock()
	if c.disks == nil {
		c.disks = map[string]*diskState{}
	}
	d := c.disks[name]
	if d == nil {
		d = &diskState{}
		c.disks[name] = d
	}
	if d.measuring || (!force && d.usage != nil && time.Since(d.usage.CheckedAt) < diskInterval) {
		c.mu.Unlock()
		return
	}
	d.measuring = true
	base := c.ctx
	c.mu.Unlock()
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx, cancel := context.WithTimeout(base, diskTimeout)
		defer cancel()
		u, err := c.Housekeeper.DiskUsage(ctx, h)
		if err != nil {
			u = &hosts.DiskUsage{Error: err.Error()}
		}
		u.CheckedAt = time.Now()
		c.recordDisk(h, u)
	}()
}

// recordDisk keeps u as h's disk usage and sends host-disk when it crosses
// h's warning threshold. A measurement without free space leaves the warning
// as it was.
func (c *Controller) recordDisk(h *hosts.Host, u *hosts.DiskUsage) {
	name := h.Name()
	if u.Error != "" {
		c.Log.Warn("measuring a host's disk", "host", name, "error", u.Error)
	}
	var ev *Event
	c.mu.Lock()
	d := c.disks[name]
	if d == nil {
		d = &diskState{}
		c.disks[name] = d
	}
	d.usage, d.measuring = u, false
	pct := diskWarningPercent(h.Spec.Spec.Housekeeping)
	if ratio, ok := u.UsedRatio(); ok {
		over := pct > 0 && ratio*100 >= float64(pct)
		if over && !d.warned {
			ev = &Event{Type: config.EventHostDisk, Host: name, Message: fmt.Sprintf("%s's disk is %.0f%% full (warning at %d%%): %s free, %s reclaimable by pruning",
				name, ratio*100, pct, hosts.FormatBytes(u.Available), hosts.FormatBytes(u.Reclaimable()))}
		}
		d.warned = over
	}
	c.mu.Unlock()
	c.publish("host", name)
	if ev != nil {
		c.notify(*ev)
	}
}

func diskWarningPercent(hk *config.Housekeeping) int {
	if hk == nil || hk.DiskWarning == nil {
		return 0
	}
	return hk.DiskWarning.Percent
}

// diskWarning says what is wrong with u under hk's threshold, or "".
func diskWarning(hk *config.Housekeeping, u *hosts.DiskUsage) string {
	pct := diskWarningPercent(hk)
	ratio, ok := u.UsedRatio()
	if pct == 0 || !ok || ratio*100 < float64(pct) {
		return ""
	}
	return fmt.Sprintf("the disk is %.0f%% full (warning at %d%%)", ratio*100, pct)
}

// housekeepingStatus fills in hs's disk usage and next scheduled prunes.
func (c *Controller) housekeepingStatus(h *hosts.Host, hs *HostStatus, now time.Time) {
	hk := h.Spec.Spec.Housekeeping
	if hk == nil || c.Housekeeper == nil {
		return
	}
	c.mu.RLock()
	if d := c.disks[h.Name()]; d != nil && d.usage != nil {
		u := *d.usage
		hs.Disk = &u
	}
	c.mu.RUnlock()
	if hs.Disk != nil {
		hs.DiskWarning = diskWarning(hk, hs.Disk)
	}
	for _, j := range pruneJobs([]*hosts.Host{h}) {
		if hs.NextPrune == nil {
			hs.NextPrune = map[string]time.Time{}
		}
		hs.NextPrune[j.kind] = j.sched.Next(now.In(j.loc))
	}
}

// pruneJob is one host's scheduled prune.
type pruneJob struct {
	host, kind string
	sched      cron.Schedule
	loc        *time.Location
	// key changes when the schedule does.
	key string
}

// pruneJobs lists hs's scheduled prunes.
func pruneJobs(hs []*hosts.Host) []pruneJob {
	var jobs []pruneJob
	add := func(host, kind, schedule, tz string) {
		if schedule == "" {
			return
		}
		sched, err := config.ParseSchedule(schedule)
		if err != nil {
			return // validation reports it
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return
		}
		jobs = append(jobs, pruneJob{host: host, kind: kind, sched: sched, loc: loc,
			key: strings.Join([]string{host, kind, schedule, tz}, "\x00")})
	}
	for _, h := range hs {
		hk := h.Spec.Spec.Housekeeping
		if hk == nil {
			continue
		}
		if p := hk.ImagePrune; p != nil {
			add(h.Name(), store.KindImagePrune, p.Schedule, p.TimeZone)
		}
		if p := hk.BuilderPrune; p != nil {
			add(h.Name(), store.KindBuilderPrune, p.Schedule, p.TimeZone)
		}
	}
	return jobs
}

// pruneClock remembers when each scheduled prune is next due.
type pruneClock struct {
	next map[string]time.Time
}

// due returns the jobs whose time has come by now, and when the next one is
// due (zero if none is scheduled). A job seen for the first time, or whose
// schedule changed, is next due after now: prunes missed while composecd
// was down aren't caught up.
func (p *pruneClock) due(now time.Time, jobs []pruneJob) ([]pruneJob, time.Time) {
	if p.next == nil {
		p.next = map[string]time.Time{}
	}
	var due []pruneJob
	var wake time.Time
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.key] = true
		at, ok := p.next[j.key]
		if !ok {
			at = j.sched.Next(now.In(j.loc))
		} else if !at.After(now) {
			due = append(due, j)
			at = j.sched.Next(now.In(j.loc))
		}
		p.next[j.key] = at
		if wake.IsZero() || at.Before(wake) {
			wake = at
		}
	}
	for k := range p.next {
		if !seen[k] {
			delete(p.next, k)
		}
	}
	return due, wake
}

// runHousekeeping runs each host's scheduled prunes until ctx is done.
func (c *Controller) runHousekeeping(ctx context.Context) {
	if c.Housekeeper == nil {
		return
	}
	var clock pruneClock
	for {
		due, next := clock.due(time.Now(), pruneJobs(c.Hosts.All()))
		byHost := map[string][]string{}
		for _, j := range due {
			byHost[j.host] = append(byHost[j.host], j.kind)
		}
		for host, kinds := range byHost {
			c.ops.Add(1)
			go func() {
				defer c.ops.Done()
				c.scheduledPrune(ctx, host, kinds)
			}()
		}
		wait := housekeepingTick
		if !next.IsZero() {
			wait = min(wait, time.Until(next))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// scheduledPrune runs host's due prunes one after another, unless the host is
// down or in maintenance.
func (c *Controller) scheduledPrune(ctx context.Context, host string, kinds []string) {
	h := c.Hosts.Get(host)
	if h == nil {
		return
	}
	if s := h.Status(); s.State != hosts.StateUp {
		c.Log.Info("skipping scheduled prune: the host is not up", "host", host, "state", s.State)
		return
	}
	if st, err := c.Store.HostState(host); err != nil || st.Maintenance {
		c.Log.Info("skipping scheduled prune: the host is in maintenance", "host", host, "error", err)
		return
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		op, err := c.startHostOperation(host, kind, store.TriggerAuto, "")
		if err != nil {
			c.Log.Warn("skipping scheduled prune", "host", host, "kind", kind, "error", err)
			continue
		}
		c.runPrune(ctx, h, op, false)
	}
}

// PruneHost starts pruning host's unused images (store.KindImagePrune) or
// build cache (store.KindBuilderPrune), with the options in its housekeeping
// section; all also removes tagged images or all unused cache. Like other
// manual operations, it runs in maintenance too.
func (c *Controller) PruneHost(host, kind string, all bool, user string) (*store.Operation, error) {
	if c.Housekeeper == nil {
		return nil, errors.New("housekeeping is not available")
	}
	h := c.Hosts.Get(host)
	if h == nil {
		return nil, fmt.Errorf("host %q: %w", host, ErrNotFound)
	}
	if kind != store.KindImagePrune && kind != store.KindBuilderPrune {
		return nil, fmt.Errorf("unknown prune kind %q", kind)
	}
	op, err := c.startHostOperation(host, kind, store.TriggerManual, user)
	if err != nil {
		return nil, err
	}
	// The caller gets a copy: the prune may finish op before it is encoded.
	started := *op
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		c.runPrune(c.baseContext(), h, op, all)
	}()
	return &started, nil
}

// HostOperations returns host's newest housekeeping operations.
func (c *Controller) HostOperations(host string, limit int) ([]*store.Operation, error) {
	if c.Hosts.Get(host) == nil {
		return nil, fmt.Errorf("host %q: %w", host, ErrNotFound)
	}
	return c.Store.HostOperations(host, limit)
}

// startHostOperation records a running housekeeping operation on host.
func (c *Controller) startHostOperation(host, kind, trigger, user string) (*store.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hostOps[host] != 0 {
		return nil, fmt.Errorf("%s: %w", host, ErrHostBusy)
	}
	if c.selfUpdating != "" || len(c.watchedHelpers) > 0 {
		return nil, fmt.Errorf("%w: composecd is updating itself", ErrBusy)
	}
	op := &store.Operation{Host: host, Kind: kind, Trigger: trigger, User: user}
	if err := c.Store.StartOperation(op); err != nil {
		return nil, err
	}
	if c.hostOps == nil {
		c.hostOps = map[string]int64{}
	}
	c.hostOps[host] = op.ID
	c.liveLogs[op.ID] = &logBuffer{max: maxLog}
	c.publish("host", host)
	return op, nil
}

// runPrune waits for h's operations to finish, keeps new ones from starting,
// prunes, and finishes op.
func (c *Controller) runPrune(ctx context.Context, h *hosts.Host, op *store.Operation, all bool) {
	log := c.opLog(op)
	o, run := pruneOptions(h.Spec.Spec.Housekeeping, op.Kind, all), c.Housekeeper.PruneImages
	if op.Kind == store.KindBuilderPrune {
		run = c.Housekeeper.PruneBuildCache
	}
	wait, cancel := context.WithTimeout(ctx, pruneWait)
	release, err := h.Exclusive(wait, prunePoll)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		op.Phase, op.Message = store.PhaseFailed, "interrupted: composecd is shutting down"
	case err != nil:
		op.Phase, op.Message = store.PhaseFailed, fmt.Sprintf("skipped: operations on %s were still running after %s", h.Name(), pruneWait)
	default:
		runCtx, cancel := context.WithTimeout(ctx, pruneTimeout)
		err = run(runCtx, h, o, log)
		cancel()
		release()
		if err != nil {
			op.Phase, op.Message = store.PhaseFailed, err.Error()
		} else {
			op.Phase, op.Message = store.PhaseSucceeded, reclaimed(log.String())
		}
	}
	op.Log = log.String()
	c.finishHostOperation(h.Name(), op)
	if op.Phase == store.PhaseSucceeded {
		c.measureDisk(h, true)
	}
}

// pruneOptions are a prune's options from the host's housekeeping section.
func pruneOptions(hk *config.Housekeeping, kind string, all bool) hosts.PruneOptions {
	o := hosts.PruneOptions{All: all}
	if hk == nil {
		return o
	}
	if p := hk.ImagePrune; kind == store.KindImagePrune && p != nil {
		o.All = all || p.All
		o.Until = time.Duration(p.Until)
	}
	if p := hk.BuilderPrune; kind == store.KindBuilderPrune && p != nil {
		o.KeepStorage = p.KeepStorage
	}
	return o
}

// reclaimed sums up a prune's output by its last line: image prune's "Total
// reclaimed space: 1.2GB", or builder prune's "Total:	1.2GB".
func reclaimed(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		for _, prefix := range []string{"Total reclaimed space:", "Total:"} {
			if rest, ok := strings.CutPrefix(lines[i], prefix); ok {
				return "reclaimed " + strings.TrimSpace(rest)
			}
		}
	}
	return "pruned"
}

func (c *Controller) finishHostOperation(host string, op *store.Operation) {
	if err := c.Store.FinishOperation(op); err != nil {
		c.Log.Error("recording an operation", "host", host, "id", op.ID, "error", err)
	}
	c.mu.Lock()
	delete(c.hostOps, host)
	if b := c.liveLogs[op.ID]; b != nil {
		delete(c.liveLogs, op.ID)
		b.finish()
	}
	c.countLocked(op)
	c.mu.Unlock()
	c.publish("host", host)
	c.publish("operation", "")
	c.Log.Info("operation finished", "host", host, "id", op.ID, "kind", op.Kind, "trigger", op.Trigger,
		"user", op.User, "phase", op.Phase, "message", op.Message)
}
