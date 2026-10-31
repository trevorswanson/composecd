package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/source"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

// maxLog bounds the output kept per operation.
const maxLog = 256 << 10

// startOperation records a running operation for app, failing with ErrBusy if
// one is already running.
func (c *Controller) startOperation(app, kind, trigger, revision, user string) (*store.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[app] != 0 {
		return nil, ErrBusy
	}
	if (c.selfUpdating != "" && c.selfUpdating != app) || len(c.watchedHelpers) > 0 {
		return nil, fmt.Errorf("%w: composecd is updating itself", ErrBusy)
	}
	op := &store.Operation{App: app, Kind: kind, Trigger: trigger, SourceRevision: revision, User: user}
	if err := c.Store.StartOperation(op); err != nil {
		return nil, err
	}
	c.running[app] = op.ID
	c.liveLogs[op.ID] = &logBuffer{max: maxLog}
	c.publish("operation", app)
	return op, nil
}

// opLog is the output buffer of running operation op.
func (c *Controller) opLog(op *store.Operation) *logBuffer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if b := c.liveLogs[op.ID]; b != nil {
		return b
	}
	return &logBuffer{max: maxLog}
}

func (c *Controller) finishOperation(app string, op *store.Operation) {
	op.Log = c.Mask(app, op.Log)
	op.Message = c.Mask(app, op.Message)
	if err := c.Store.FinishOperation(op); err != nil {
		c.Log.Error("recording an operation", "app", app, "id", op.ID, "error", err)
	}
	c.mu.Lock()
	delete(c.running, app)
	if b := c.liveLogs[op.ID]; b != nil {
		delete(c.liveLogs, op.ID)
		b.finish()
	}
	k := metricKey{op.Kind, op.Trigger, op.Phase}
	m := c.metrics[k]
	if m == nil {
		m = &opMetric{}
		c.metrics[k] = m
	}
	m.Count++
	if op.FinishedAt != nil {
		m.Seconds += op.FinishedAt.Sub(op.StartedAt).Seconds()
	}
	host := ""
	if s, ok := c.apps[app]; ok {
		host = s.status.Host
	}
	c.mu.Unlock()
	c.publish("operation", app)
	c.Log.Info("operation finished", "app", app, "id", op.ID, "kind", op.Kind, "trigger", op.Trigger,
		"user", op.User, "phase", op.Phase, "message", op.Message)

	ev := Event{App: app, Host: host, Revision: op.SourceRevision}
	what := op.Kind
	if op.Trigger == store.TriggerAuto && op.Kind == store.KindSync {
		what = "automatic sync"
	}
	if op.User != "" {
		what += " by " + op.User
	}
	switch {
	case op.Phase == store.PhaseFailed:
		ev.Type, ev.Message = config.EventSyncFailed, fmt.Sprintf("%s of %s failed: %s", what, app, firstLine(op.Message))
	case op.Kind == store.KindHeal:
		ev.Type, ev.Message = config.EventHealed, fmt.Sprintf("%s self-healed: %s", app, op.Message)
	case op.Kind == store.KindPrune:
		ev.Type, ev.Message = config.EventPruned, fmt.Sprintf("%s pruned: %s", app, op.Message)
	default:
		ev.Type, ev.Message = config.EventSyncSucceeded, fmt.Sprintf("%s of %s succeeded: %s", what, app, op.Message)
	}
	c.notify(ev)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func deployOptions(o config.SyncOptions) action.DeployOptions {
	return action.DeployOptions{
		Wait: o.WaitOrDefault(), Timeout: o.TimeoutOrDefault(), Pull: o.Pull, ForceRecreate: o.ForceRecreate,
	}
}

// runSync upgrades (or installs) the release to p and fills in op's outcome.
func (c *Controller) runSync(ctx context.Context, p *plan, op *store.Operation) {
	defer p.host.Use()()
	name := p.app.Metadata.Name
	log := c.opLog(op)
	cfg := p.host.Configuration(p.prep.Env, log)
	cfg.Registry = c.Registry
	u := c.newUpgrade(cfg, p, op)

	op.SourceRevision = p.prep.Revision
	// Upgrade.Run, with the images pinned as assess pinned them in between.
	rel, err := c.prepareUpgrade(ctx, cfg, u, p)
	if err == nil {
		a := action.NewApply(cfg)
		a.DeployOptions, a.Atomic, a.HistoryMax, a.Description = u.DeployOptions, u.Atomic, u.HistoryMax, u.Description
		rel, err = a.Run(ctx, name, rel.Revision)
	}
	if rel != nil {
		op.ReleaseRevision = rel.Revision
	}
	op.Log = log.String()
	if err != nil {
		op.Phase, op.Message = store.PhaseFailed, err.Error()
		return
	}
	op.Phase, op.Message = store.PhaseSucceeded, rel.Description
}

// prepareUpgrade saves p's next revision as pending, with its moving tags
// pinned to p.pins, abandoning it if they can't be.
func (c *Controller) prepareUpgrade(ctx context.Context, cfg *action.Configuration, u *action.Upgrade, p *plan) (*release.Release, error) {
	name := p.app.Metadata.Name
	rel, err := u.Prepare(ctx, name, p.prep.ChartRef, p.prep.Values)
	if err != nil {
		return rel, err
	}
	if err := pinRelease(ctx, cfg, rel, p.pins); err != nil {
		err = fmt.Errorf("pinning images: %w", err)
		if aerr := action.Abandon(cfg, name, rel.Revision, err.Error()); aerr != nil {
			c.Log.Error("abandoning a revision whose images couldn't be pinned", "app", name, "error", aerr)
		}
		return rel, err
	}
	return rel, nil
}

// newUpgrade is the upgrade (or install) that syncs p as operation op.
func (c *Controller) newUpgrade(cfg *action.Configuration, p *plan, op *store.Operation) *action.Upgrade {
	name := p.app.Metadata.Name
	o := p.app.Spec.SyncPolicy.Options
	u := action.NewUpgrade(cfg)
	u.Install, u.ResetValues = true, true
	u.DeployOptions = deployOptions(o)
	u.Atomic, u.HistoryMax = o.Atomic, o.HistoryMaxOrDefault()
	u.Env, u.ChartVersion = p.prep.Lookup, p.prep.Version
	if p.lookup != nil {
		u.Lookup = p.lookup.Func
	}
	u.ProjectDir = p.host.ProjectDir(name)
	u.Description = "composecd sync"
	if p.prep.Revision != "" {
		u.Description += " to " + short(p.prep.Revision)
	}
	u.Labels = map[string]string{
		LabelManagedBy: ManagedBy, LabelApp: name,
		// "" removes the label, for an oci:// chart that has no commit.
		LabelSourceRevision: p.prep.Revision,
		LabelValuesRevision: p.prep.ValuesRevision,
		LabelApplicationSet: p.app.Metadata.Labels[config.LabelApplicationSet],
		LabelOperationID:    fmt.Sprint(op.ID),
		// "" removes it when prune is off.
		LabelPrune: "",
	}
	if a := p.app.Spec.SyncPolicy.Automated; a != nil && a.Prune {
		u.Labels[LabelPrune] = "true"
	}
	// Compose may need the project directory to exist where it runs; on a remote
	// host this creates an empty directory on the controller, which is harmless.
	_ = os.MkdirAll(u.ProjectDir, 0o755)
	return u
}

// Sync starts syncing app to Git (fetching its repository first) and returns
// the running operation. It works even when automatic sync is paused.
// user is who asked, recorded on the operation.
func (c *Controller) Sync(name, user string) (*store.Operation, error) {
	app, err := c.application(name)
	if err != nil {
		return nil, err
	}
	if err := c.manualAllowed(app); err != nil {
		return nil, err
	}
	op, err := c.startOperation(name, store.KindSync, store.TriggerManual, "", user)
	if err != nil {
		return nil, err
	}
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx := c.baseContext()
		fetchErr := c.fetchSource(ctx, app)
		defer c.lockApp(name)()
		st, p := c.assess(ctx, app, fetchErr)
		if p == nil {
			op.Phase, op.Message = store.PhaseFailed, st.Error
			c.finishOperation(name, op)
			c.setStatus(st)
			return
		}
		c.sync(ctx, p, op)
		p.done()
		c.finishOperation(name, op)
		c.mu.Lock()
		if s, ok := c.apps[name]; ok {
			// Auto-sync won't retry this state if the manual sync failed.
			s.attempted, s.failures, s.nextRetry = p.key, 0, time.Time{}
		}
		c.mu.Unlock()
		st, p = c.assess(ctx, app, nil)
		p.done()
		c.setStatus(st)
	}()
	return op, nil
}

// manualAllowed checks the sync windows for a manual operation on app, and
// refuses one on composecd itself.
func (c *Controller) manualAllowed(app *config.Application) error {
	if c.isSelfApp(app) && !c.canUpdateSelf() {
		return fmt.Errorf("%w: %s", ErrSelf, selfBlocked)
	}
	c.mu.RLock()
	ws := c.windows
	c.mu.RUnlock()
	d := windows.Evaluate(ws, app.Metadata.Name, app.Spec.Destination.Host, app.Metadata.Labels, time.Now(), true)
	if !d.Allowed {
		return fmt.Errorf("%w: %s (the window doesn't allow manual syncs)", ErrBlocked, d.Reason)
	}
	return nil
}

// Rollback starts redeploying an earlier composetpl revision of app (0: the one
// before the latest). An automated app is paused afterwards, as Argo CD does,
// so auto-sync doesn't undo the rollback.
// user is who asked, recorded on the operation.
func (c *Controller) Rollback(name string, revision int, user string) (*store.Operation, error) {
	app, err := c.application(name)
	if err != nil {
		return nil, err
	}
	h := c.Hosts.Get(app.Spec.Destination.Host)
	if h == nil {
		return nil, fmt.Errorf("host %q is not defined", app.Spec.Destination.Host)
	}
	if err := c.manualAllowed(app); err != nil {
		return nil, err
	}
	op, err := c.startOperation(name, store.KindRollback, store.TriggerManual, "", user)
	if err != nil {
		return nil, err
	}
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx := c.baseContext()
		defer c.lockApp(name)()
		if c.isSelfApp(app) {
			c.runSelfRollback(ctx, app, h, op, revision)
			c.finishOperation(name, op)
			st, p := c.assess(ctx, app, nil)
			p.done()
			c.setStatus(st)
			return
		}
		func() {
			defer h.Use()()
			log := c.opLog(op)
			if _, err := c.releaseHistory(h, name); err != nil {
				op.Phase, op.Message = store.PhaseFailed, "reading release history: "+err.Error()
				return
			}
			cfg := h.Configuration(source.Env(h.Spec, app), log)
			o := app.Spec.SyncPolicy.Options
			rb := action.NewRollback(cfg)
			rb.Version, rb.DeployOptions, rb.HistoryMax = revision, deployOptions(o), o.HistoryMaxOrDefault()
			rb.Labels = map[string]string{LabelOperationID: fmt.Sprint(op.ID)}
			rel, err := rb.Run(ctx, name)
			op.Log = log.String()
			if rel != nil {
				op.ReleaseRevision = rel.Revision
				op.SourceRevision = rel.Labels[LabelSourceRevision]
			}
			if err != nil {
				op.Phase, op.Message = store.PhaseFailed, err.Error()
				return
			}
			op.Phase, op.Message = store.PhaseSucceeded, rel.Description
			if app.Spec.SyncPolicy.Automated != nil {
				reason := fmt.Sprintf("rolled back (%s); resume to sync from Git again", rel.Description)
				if err := c.Store.SetPaused(name, true, reason); err != nil {
					c.Log.Error("pausing after rollback", "app", name, "error", err)
				}
			}
		}()
		c.finishOperation(name, op)
		st, p := c.assess(ctx, app, nil)
		p.done()
		c.setStatus(st)
	}()
	return op, nil
}

// Pause stops automatic sync of app until Resume.
func (c *Controller) Pause(name, reason string) error {
	if _, err := c.application(name); err != nil {
		return err
	}
	if reason == "" {
		reason = "paused"
	}
	if err := c.Store.SetPaused(name, true, reason); err != nil {
		return err
	}
	c.updateStatus(name, func(st *AppStatus) { st.Paused, st.PausedReason = true, reason })
	return nil
}

// Resume re-enables automatic sync of app and reconciles it.
func (c *Controller) Resume(name string) error {
	app, err := c.application(name)
	if err != nil {
		return err
	}
	if err := c.Store.SetPaused(name, false, ""); err != nil {
		return err
	}
	c.mu.Lock()
	if s, ok := c.apps[name]; ok {
		s.attempted, s.failures, s.nextRetry, s.heals, s.nextHeal = "", 0, time.Time{}, 0, time.Time{}
		s.status.Paused, s.status.PausedReason = false, ""
	}
	c.mu.Unlock()
	c.publish("app", name)
	c.reconcileAsync(app, true)
	return nil
}

// RefreshApp fetches app's repository and reconciles it in the background.
func (c *Controller) RefreshApp(name string) error {
	app, err := c.application(name)
	if err != nil {
		return err
	}
	c.reconcileAsync(app, true)
	return nil
}

// reconcileAsync reconciles app in the background, fetching its repository
// first if fetch.
func (c *Controller) reconcileAsync(app *config.Application, fetch bool) {
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx := c.baseContext()
		var fetchErr error
		if fetch {
			fetchErr = c.fetchSource(ctx, app)
		}
		c.sem <- struct{}{}
		defer func() { <-c.sem }()
		c.reconcile(ctx, app, fetchErr, true)
	}()
}

func (c *Controller) updateStatus(name string, f func(*AppStatus)) {
	defer c.publish("app", name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.apps[name]; ok {
		f(&s.status)
	}
}

// History returns app's composetpl revisions, oldest first.
func (c *Controller) History(name string) ([]Release, error) {
	app, err := c.application(name)
	if err != nil {
		return nil, err
	}
	h := c.Hosts.Get(app.Spec.Destination.Host)
	if h == nil {
		return nil, fmt.Errorf("host %q is not defined", app.Spec.Destination.Host)
	}
	defer h.Use()()
	hist, err := c.releaseHistory(h, name)
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(hist))
	for _, r := range hist {
		out = append(out, masked(c, name, *releaseInfo(r)))
	}
	return out, nil
}

// Operations returns app's newest operations ("" app: every app's).
func (c *Controller) Operations(app string, limit int) ([]*store.Operation, error) {
	if app != "" {
		if _, err := c.application(app); err != nil {
			return nil, err
		}
	}
	ops, err := c.Store.Operations(app, limit)
	if err != nil {
		return nil, err
	}
	for i, op := range ops {
		copyOp := *op
		copyOp.Log = c.Mask(op.App, op.Log)
		copyOp.Message = c.Mask(op.App, op.Message)
		ops[i] = &copyOp
	}
	return ops, nil
}

// Operation returns one operation with its log.
func (c *Controller) Operation(id int64) (*store.Operation, error) {
	op, err := c.Store.Operation(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("operation %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	copyOp := *op
	copyOp.Log = c.Mask(op.App, op.Log)
	copyOp.Message = c.Mask(op.App, op.Message)
	return &copyOp, nil
}

// FollowOperation writes operation id's output to w, masked line by line. A
// running operation's output is followed until it finishes or ctx ends.
func (c *Controller) FollowOperation(ctx context.Context, id int64, w io.Writer) error {
	c.mu.RLock()
	b := c.liveLogs[id]
	c.mu.RUnlock()
	op, err := c.Store.Operation(id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("operation %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if starter, ok := w.(interface{ Start() }); ok {
		starter.Start()
	}
	if b == nil {
		_, err := io.WriteString(w, c.Mask(op.App, op.Log))
		return err
	}
	var pending []byte
	for off := 0; ; {
		data, changed, done := b.since(off)
		off += len(data)
		pending = append(pending, data...)
		if i := bytes.LastIndexByte(pending, '\n'); i >= 0 {
			if _, err := io.WriteString(w, c.Mask(op.App, string(pending[:i+1]))); err != nil {
				return err
			}
			pending = pending[i+1:]
		}
		if done {
			if len(pending) > 0 {
				_, err := io.WriteString(w, c.Mask(op.App, string(pending)))
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// logBuffer keeps the first max bytes written to it, concurrently, and lets
// readers follow it until finish.
type logBuffer struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
	finished  bool
	// changed is closed, and replaced, on every write and on finish.
	changed chan struct{}
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - len(b.buf); room < len(p) {
		if room > 0 {
			b.buf = append(b.buf, p[:room]...)
		}
		b.truncated = true
	} else {
		b.buf = append(b.buf, p...)
	}
	b.wakeLocked()
	return len(p), nil
}

func (b *logBuffer) wakeLocked() {
	if b.changed != nil {
		close(b.changed)
	}
	b.changed = make(chan struct{})
}

// finish tells followers no more output is coming.
func (b *logBuffer) finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finished = true
	b.wakeLocked()
}

// since returns the output after off, a channel closed when there is more,
// and whether the operation finished.
func (b *logBuffer) since(off int) ([]byte, <-chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	data := append([]byte(nil), b.buf[min(off, len(b.buf)):]...)
	if b.finished && b.truncated {
		data = append(data, "\n[output truncated]\n"...)
	}
	return data, b.changed, b.finished
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return string(b.buf) + "\n[output truncated]\n"
	}
	return string(b.buf)
}
