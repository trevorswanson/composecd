package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/source"
	"github.com/trevorswanson/composecd/internal/store"
)

// maxLog bounds the output kept per operation.
const maxLog = 256 << 10

// startOperation records a running operation for app, failing with ErrBusy if
// one is already running.
func (c *Controller) startOperation(app, kind, trigger, revision string) (*store.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[app] != 0 {
		return nil, ErrBusy
	}
	op := &store.Operation{App: app, Kind: kind, Trigger: trigger, SourceRevision: revision}
	if err := c.Store.StartOperation(op); err != nil {
		return nil, err
	}
	c.running[app] = op.ID
	return op, nil
}

func (c *Controller) finishOperation(app string, op *store.Operation) {
	if err := c.Store.FinishOperation(op); err != nil {
		c.Log.Error("recording an operation", "app", app, "id", op.ID, "error", err)
	}
	c.mu.Lock()
	delete(c.running, app)
	c.mu.Unlock()
	c.Log.Info("operation finished", "app", app, "id", op.ID, "kind", op.Kind, "trigger", op.Trigger,
		"phase", op.Phase, "message", op.Message)
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
	o := p.app.Spec.SyncPolicy.Options
	log := &logBuffer{max: maxLog}
	cfg := p.host.Configuration(p.prep.Env, log)

	u := action.NewUpgrade(cfg)
	u.Install, u.ResetValues = true, true
	u.DeployOptions = deployOptions(o)
	u.Atomic, u.HistoryMax = o.Atomic, o.HistoryMaxOrDefault()
	u.Env, u.ChartVersion = p.prep.Lookup, p.prep.Version
	u.ProjectDir = p.host.ProjectDir(name)
	u.Description = "composecd sync"
	if p.prep.Revision != "" {
		u.Description += " to " + short(p.prep.Revision)
	}
	u.Labels = map[string]string{
		LabelManagedBy: ManagedBy, LabelApp: name,
		// "" removes the label, for an oci:// chart that has no commit.
		LabelSourceRevision: p.prep.Revision,
		LabelOperationID:    fmt.Sprint(op.ID),
	}
	// Compose may need the project directory to exist where it runs; on a remote
	// host this creates an empty directory on the controller, which is harmless.
	_ = os.MkdirAll(u.ProjectDir, 0o755)

	op.SourceRevision = p.prep.Revision
	rel, err := u.Run(ctx, name, p.prep.ChartRef, p.prep.Values)
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

// Sync starts syncing app to Git (fetching its repository first) and returns
// the running operation. It works even when automatic sync is paused.
func (c *Controller) Sync(name string) (*store.Operation, error) {
	app, err := c.application(name)
	if err != nil {
		return nil, err
	}
	op, err := c.startOperation(name, store.KindSync, store.TriggerManual, "")
	if err != nil {
		return nil, err
	}
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx := c.baseContext()
		var fetchErr error
		if !app.Spec.Source.IsOCI() {
			fetchErr = c.Git.Fetch(ctx, app.Spec.Source.RepoURL)
		}
		defer c.lockApp(name)()
		st, p := c.assess(ctx, app, fetchErr)
		if p == nil {
			op.Phase, op.Message = store.PhaseFailed, st.Error
			c.finishOperation(name, op)
			c.setStatus(st)
			return
		}
		c.runSync(ctx, p, op)
		p.done()
		c.finishOperation(name, op)
		c.mu.Lock()
		if s, ok := c.apps[name]; ok {
			// Auto-sync won't retry this state if the manual sync failed.
			s.attempted = p.key
		}
		c.mu.Unlock()
		st, p = c.assess(ctx, app, nil)
		p.done()
		c.setStatus(st)
	}()
	return op, nil
}

// Rollback starts redeploying an earlier composetpl revision of app (0: the one
// before the latest). An automated app is paused afterwards, as Argo CD does,
// so auto-sync doesn't undo the rollback.
func (c *Controller) Rollback(name string, revision int) (*store.Operation, error) {
	app, err := c.application(name)
	if err != nil {
		return nil, err
	}
	h := c.Hosts.Get(app.Spec.Destination.Host)
	if h == nil {
		return nil, fmt.Errorf("host %q is not defined", app.Spec.Destination.Host)
	}
	op, err := c.startOperation(name, store.KindRollback, store.TriggerManual, "")
	if err != nil {
		return nil, err
	}
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx := c.baseContext()
		defer c.lockApp(name)()
		func() {
			defer h.Use()()
			log := &logBuffer{max: maxLog}
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
		s.attempted = ""
		s.status.Paused, s.status.PausedReason = false, ""
	}
	c.mu.Unlock()
	c.reconcileAsync(app)
	return nil
}

// RefreshApp fetches app's repository and reconciles it in the background.
func (c *Controller) RefreshApp(name string) error {
	app, err := c.application(name)
	if err != nil {
		return err
	}
	c.reconcileAsync(app)
	return nil
}

func (c *Controller) reconcileAsync(app *config.Application) {
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		ctx := c.baseContext()
		var fetchErr error
		if !app.Spec.Source.IsOCI() {
			fetchErr = c.Git.Fetch(ctx, app.Spec.Source.RepoURL)
		}
		c.sem <- struct{}{}
		defer func() { <-c.sem }()
		c.reconcile(ctx, app, fetchErr, true)
	}()
}

func (c *Controller) updateStatus(name string, f func(*AppStatus)) {
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
	hist, err := h.Store().History(name)
	if errors.Is(err, storage.ErrNotFound) {
		return []Release{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(hist))
	for _, r := range hist {
		out = append(out, *releaseInfo(r))
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
	return c.Store.Operations(app, limit)
}

// Operation returns one operation with its log.
func (c *Controller) Operation(id int64) (*store.Operation, error) {
	op, err := c.Store.Operation(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("operation %d: %w", id, ErrNotFound)
	}
	return op, err
}

// logBuffer keeps the first max bytes written to it, concurrently.
type logBuffer struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
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
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return string(b.buf) + "\n[output truncated]\n"
	}
	return string(b.buf)
}
