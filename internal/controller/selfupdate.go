package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/selfapply"
	"github.com/trevorswanson/composecd/internal/source"
	"github.com/trevorswanson/composecd/internal/store"
)

// Helpers start and wait for the container that applies composecd's own
// release (docs/SELF.md); hosts.CLI implements it. Without Helpers, composecd
// doesn't update itself.
type Helpers interface {
	StartHelper(ctx context.Context, h *hosts.Host, spec hosts.HelperSpec) error
	// WaitHelper blocks until the helper exits and returns its exit code.
	WaitHelper(ctx context.Context, h *hosts.Host, name string) (int, error)
	// ListHelpers returns h's containers with label, running or not.
	ListHelpers(ctx context.Context, h *hosts.Host, label string) ([]hosts.HelperInfo, error)
}

// Labels on a helper container: the operation it carries out, and the app.
const (
	labelSelfApply    = "composecd.self-apply"
	labelSelfApplyApp = "composecd.self-apply.app"
)

// helperMaxAge is how long a helper container may live. A helper finishes
// within its timeout, so an older one is stuck, or left over.
var helperMaxAge = 24 * time.Hour

// Before updating itself, composecd waits this long for other operations to
// finish, since the update restarts it.
var (
	drainTimeout = 2 * time.Minute
	drainPoll    = 200 * time.Millisecond
)

func helperName(opID int64) string { return fmt.Sprintf("composecd-self-apply-%d", opID) }

// canUpdateSelf reports whether composecd can start a helper: it knows its own
// container and image.
func (c *Controller) canUpdateSelf() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Helpers != nil && c.SelfContainer != "" && c.selfImage != ""
}

// sync syncs p, through a helper when p is composecd itself.
func (c *Controller) sync(ctx context.Context, p *plan, op *store.Operation) {
	if c.isSelfApp(p.app) {
		c.runSelfSync(ctx, p, op)
		return
	}
	c.runSync(ctx, p, op)
}

// heal self-heals p, through a helper when p is composecd itself.
func (c *Controller) heal(ctx context.Context, p *plan, op *store.Operation, drift string) {
	if c.isSelfApp(p.app) {
		c.runSelfHeal(ctx, p, op, drift)
		return
	}
	c.runHeal(ctx, p, op, drift)
}

// beginSelfUpdate keeps other operations from starting and waits, up to
// drainTimeout, for the running ones to finish. On success, endSelfUpdate
// lifts the hold.
func (c *Controller) beginSelfUpdate(ctx context.Context, self string) error {
	c.mu.Lock()
	c.selfUpdating = self
	c.mu.Unlock()
	deadline := time.Now().Add(drainTimeout)
	for {
		c.mu.RLock()
		busy := ""
		for app := range c.running {
			if app != self {
				busy = app
				break
			}
		}
		c.mu.RUnlock()
		if busy == "" {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			c.endSelfUpdate()
			return fmt.Errorf("the operation on %s didn't finish within %s; composecd updates itself only when nothing else is running", busy, drainTimeout)
		}
		select {
		case <-ctx.Done():
		case <-time.After(drainPoll):
		}
	}
}

func (c *Controller) endSelfUpdate() {
	c.mu.Lock()
	c.selfUpdating = ""
	c.mu.Unlock()
}

// helperOptions are the self-apply options for app on h, with env added to
// its docker calls.
func (c *Controller) helperOptions(h *hosts.Host, app *config.Application, env []string) selfapply.Options {
	o := app.Spec.SyncPolicy.Options
	d := deployOptions(o)
	// The helper decides between done and rolled back by whether the new
	// composecd turns healthy.
	d.Wait = true
	so := selfapply.Options{Release: app.Metadata.Name, RollbackTo: -1, Deploy: d, HistoryMax: o.HistoryMaxOrDefault(),
		Storage: h.Spec.Spec.Storage, Env: append(h.Env(), env...)}
	if so.Storage == config.StorageLocal {
		so.LocalDir = filepath.Join(c.Boot.DataDir, "hosts", h.Name(), "releases")
	}
	return so
}

// helperEnv selects the engine for the helper's docker calls as composecd's
// own calls to h select it. The helper shares composecd's volumes, so the
// socket, TLS certificates and SSH keys composecd uses are there too.
func helperEnv(h *hosts.Host) []string {
	var env []string
	switch {
	case h.Spec.Spec.URL != "":
		env = append(env, "DOCKER_HOST="+h.Spec.Spec.URL)
	case h.Spec.Spec.Context != "":
		env = append(env, "DOCKER_CONTEXT="+h.Spec.Spec.Context)
	case os.Getenv("DOCKER_HOST") != "":
		env = append(env, "DOCKER_HOST="+os.Getenv("DOCKER_HOST"))
	}
	if img := os.Getenv("COMPOSECD_HELPER_IMAGE"); img != "" {
		env = append(env, "COMPOSECD_HELPER_IMAGE="+img)
	}
	return env
}

// runSelfSync prepares p's release and has a helper apply it.
func (c *Controller) runSelfSync(ctx context.Context, p *plan, op *store.Operation) {
	name := p.app.Metadata.Name
	if err := c.beginSelfUpdate(ctx, name); err != nil {
		op.Phase, op.Message = store.PhaseFailed, err.Error()
		return
	}
	func() {
		defer p.host.Use()()
		log := c.opLog(op)
		cfg := p.host.Configuration(p.prep.Env, log)
		cfg.Registry = c.Registry
		u := c.newUpgrade(cfg, p, op)
		op.SourceRevision = p.prep.Revision
		rel, err := c.prepareUpgrade(ctx, cfg, u, p)
		if err != nil {
			op.Log = log.String()
			if rel != nil {
				op.ReleaseRevision = rel.Revision
			}
			op.Phase, op.Message = store.PhaseFailed, err.Error()
			c.endSelfUpdate()
			return
		}
		if err := checkSelfConfig(rel.Manifest, c.Boot.Path); err != nil {
			op.Log = log.String()
			op.ReleaseRevision = rel.Revision
			op.Phase, op.Message = store.PhaseFailed, err.Error()
			if aerr := action.Abandon(cfg, name, rel.Revision, err.Error()); aerr != nil {
				c.Log.Error("abandoning a revision of composecd with an invalid configuration", "app", name, "error", aerr)
			}
			c.endSelfUpdate()
			return
		}
		fmt.Fprintf(log, "Prepared revision %d; a helper container applies it, since composecd restarts.\n", rel.Revision)
		so := c.helperOptions(p.host, p.app, p.prep.Env)
		so.Revision, so.Description = rel.Revision, u.Description
		c.runHelper(ctx, p.host, op, so)
	}()
	p.host.Invalidate()
}

// checkSelfConfig parses the composecd configuration that manifest mounts at
// path, where this composecd read its own, as the new composecd will read it,
// so a configuration that won't load fails before composecd restarts. It
// returns nil when nothing the manifest mounts there can be read from here.
func checkSelfConfig(manifest, path string) error {
	if path == "" {
		return nil
	}
	var doc struct {
		Services map[string]struct {
			Configs []interface{} `yaml:"configs"`
		} `yaml:"services"`
		Configs map[string]struct {
			Content *string `yaml:"content"`
		} `yaml:"configs"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return nil // compose config has already checked the manifest
	}
	for _, svc := range doc.Services {
		for _, ref := range svc.Configs {
			var source, target string
			switch r := ref.(type) {
			case string:
				source = r
			case map[string]interface{}:
				source, _ = r["source"].(string)
				target, _ = r["target"].(string)
			}
			if target == "" {
				target = "/" + source
			}
			if target != path {
				continue
			}
			cfg, ok := doc.Configs[source]
			if !ok || cfg.Content == nil {
				return nil // a file: config isn't readable from here
			}
			// Compose unescapes $$ in content.
			content := strings.ReplaceAll(*cfg.Content, "$$", "$")
			if _, err := config.ParseBootstrap([]byte(content)); err != nil {
				return fmt.Errorf("the new composecd configuration (config %q, mounted at %s) is invalid, so composecd didn't update itself: %w", source, path, err)
			}
			return nil
		}
	}
	return nil
}

// runSelfHeal has a helper repair p's drifted containers.
func (c *Controller) runSelfHeal(ctx context.Context, p *plan, op *store.Operation, drift string) {
	if err := c.beginSelfUpdate(ctx, p.app.Metadata.Name); err != nil {
		op.Phase, op.Message = store.PhaseFailed, err.Error()
		return
	}
	func() {
		defer p.host.Use()()
		so := c.helperOptions(p.host, p.app, p.prep.Env)
		so.Repair, so.Deploy.ForceRecreate = true, false
		c.runHelper(ctx, p.host, op, so)
	}()
	p.host.Invalidate()
	if op.Phase == store.PhaseSucceeded {
		op.Message = fmt.Sprintf("repaired %s to revision %d", drift, op.ReleaseRevision)
	}
}

// runSelfRollback has a helper roll app back to revision on h.
func (c *Controller) runSelfRollback(ctx context.Context, app *config.Application, h *hosts.Host, op *store.Operation, revision int) {
	if err := c.beginSelfUpdate(ctx, app.Metadata.Name); err != nil {
		op.Phase, op.Message = store.PhaseFailed, err.Error()
		return
	}
	func() {
		defer h.Use()()
		if _, err := c.releaseHistory(h, app.Metadata.Name); err != nil {
			op.Phase, op.Message = store.PhaseFailed, "reading release history: "+err.Error()
			c.endSelfUpdate()
			return
		}
		so := c.helperOptions(h, app, source.Env(h.Spec, app))
		so.RollbackTo = revision
		so.Labels = map[string]string{LabelOperationID: fmt.Sprint(op.ID)}
		c.runHelper(ctx, h, op, so)
	}()
	h.Invalidate()
	c.afterSelfOperation(app, op)
}

// runHelper starts a helper for o on h, waits for it, and fills in op.
func (c *Controller) runHelper(ctx context.Context, h *hosts.Host, op *store.Operation, o selfapply.Options) {
	name := helperName(op.ID)
	if err := c.Store.SetHelper(op.ID, name); err != nil {
		op.Phase, op.Message = store.PhaseFailed, "recording the helper container: "+err.Error()
		c.endSelfUpdate()
		return
	}
	op.Helper = name
	c.mu.RLock()
	image := c.selfImage
	c.mu.RUnlock()
	spec := hosts.HelperSpec{
		Name: name, Image: image, Args: append([]string{"self-apply"}, o.Args()...), VolumesFrom: c.SelfContainer,
		Labels: map[string]string{labelSelfApply: fmt.Sprint(op.ID), labelSelfApplyApp: op.App}, Env: helperEnv(h),
	}
	c.Log.Info("starting a helper container to update composecd", "app", op.App, "host", h.Name(), "helper", name)
	if err := c.Helpers.StartHelper(ctx, h, spec); err != nil {
		op.Log = c.opLog(op).String()
		op.Phase, op.Message = store.PhaseFailed, err.Error()
		c.endSelfUpdate()
		return
	}
	c.awaitHelper(ctx, h, op, c.opLog(op))
}

// awaitHelper waits for op's helper to exit, following its output into log,
// fills in op from its exit code and the release revision it left, and removes
// it. This process usually doesn't see the end: the helper replaces it, and the
// next one calls awaitHelper again from recoverSelf.
func (c *Controller) awaitHelper(ctx context.Context, h *hosts.Host, op *store.Operation, log *logBuffer) {
	defer c.endSelfUpdate()
	from := len(log.String())
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		_ = c.Hosts.Backend.Logs(lctx, h, op.Helper, 10000, true, log)
	}()
	code, err := c.Helpers.WaitHelper(ctx, h, op.Helper)
	// The stream ends with the container; give it a moment to catch up.
	select {
	case <-followed:
	case <-time.After(5 * time.Second):
		cancel()
		<-followed
	}
	op.Log = log.String()

	// The helper changed the release behind the host's storage cache.
	desc := ""
	if rels, herr := c.Hosts.Backend.Store(h).History(op.App); herr == nil && len(rels) > 0 {
		rel := rels[len(rels)-1]
		op.ReleaseRevision, desc = rel.Revision, rel.Description
		if op.SourceRevision == "" {
			op.SourceRevision = rel.Labels[LabelSourceRevision]
		}
	}
	switch {
	case err != nil:
		op.Phase, op.Message = store.PhaseFailed, err.Error()
	case code != 0:
		msg := fmt.Sprintf("the helper container exited with code %d", code)
		if last := lastLine(op.Log[min(from, len(op.Log)):]); last != "" {
			msg += ": " + last
		}
		op.Phase, op.Message = store.PhaseFailed, msg
	default:
		op.Phase, op.Message = store.PhaseSucceeded, desc
	}
	if err == nil {
		c.removeHelper(ctx, h, op.Helper)
	}
}

// removeHelper removes helper container name from h, stopping it if it runs.
func (c *Controller) removeHelper(ctx context.Context, h *hosts.Host, name string) {
	if err := h.Configuration(nil, io.Discard).Compose.RemoveContainer(ctx, name); err != nil {
		c.Log.Warn("removing a helper container", "host", h.Name(), "helper", name, "error", err)
	}
}

// sweepHelpers looks after the helper containers on the hosts composecd runs
// on that no operation of this composecd is waiting for: left by an operation
// it has no record of (a restored database), or whose wait failed. A running
// one may be replacing composecd right now, so every operation waits for it to
// exit; then it's removed, as an exited one is at once. A helper older than
// helperMaxAge is stuck or forgotten, and is removed, running or not.
func (c *Controller) sweepHelpers(ctx context.Context) {
	c.mu.RLock()
	ready := c.Helpers != nil && c.selfRecovered
	var on []string
	for host, project := range c.selfFound {
		if project != "" {
			on = append(on, host)
		}
	}
	c.mu.RUnlock()
	if !ready {
		return
	}
	for _, name := range on {
		h := c.Hosts.Get(name)
		if h == nil || h.Status().State != hosts.StateUp {
			continue
		}
		list, err := c.Helpers.ListHelpers(ctx, h, labelSelfApply)
		if err != nil {
			c.Log.Warn("listing helper containers", "host", name, "error", err)
			continue
		}
		for _, hi := range list {
			c.sweepHelper(ctx, h, hi)
		}
	}
}

func (c *Controller) sweepHelper(ctx context.Context, h *hosts.Host, hi hosts.HelperInfo) {
	id, _ := strconv.ParseInt(hi.Labels[labelSelfApply], 10, 64)
	app := hi.Labels[labelSelfApplyApp]
	c.mu.RLock()
	ours := (id != 0 && app != "" && c.running[app] == id) || c.watchedHelpers[hi.Name]
	c.mu.RUnlock()
	switch {
	case time.Since(hi.Created) > helperMaxAge:
		// An operation waiting for it then fails.
		c.Log.Warn("removing a helper container older than "+helperMaxAge.String(), "host", h.Name(), "helper", hi.Name, "running", hi.Running)
		c.removeHelper(ctx, h, hi.Name)
	case ours:
	case hi.Running:
		c.watchHelper(ctx, h, hi.Name)
	default:
		if _, err := c.Store.Operation(id); errors.Is(err, store.ErrNotFound) {
			c.Log.Warn("removing a helper container left by an operation composecd has no record of", "host", h.Name(), "helper", hi.Name)
		}
		c.removeHelper(ctx, h, hi.Name)
	}
}

// watchHelper holds every operation until helper container name on h exits,
// then removes it.
func (c *Controller) watchHelper(ctx context.Context, h *hosts.Host, name string) {
	c.mu.Lock()
	if c.watchedHelpers == nil {
		c.watchedHelpers = map[string]bool{}
	}
	c.watchedHelpers[name] = true
	c.mu.Unlock()
	c.Log.Warn("a helper container no operation is waiting for is updating composecd; operations wait until it exits",
		"host", h.Name(), "helper", name)
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		code, err := c.Helpers.WaitHelper(ctx, h, name)
		c.Log.Info("a helper container exited", "host", h.Name(), "helper", name, "code", code, "error", err)
		c.removeHelper(ctx, h, name)
		c.mu.Lock()
		delete(c.watchedHelpers, name)
		c.mu.Unlock()
		h.Invalidate()
	}()
}

// afterSelfOperation pauses an automated app that op rolled back, as Rollback
// does, so auto-sync doesn't undo the rollback.
func (c *Controller) afterSelfOperation(app *config.Application, op *store.Operation) {
	if app == nil || op.Kind != store.KindRollback || op.Phase != store.PhaseSucceeded || app.Spec.SyncPolicy.Automated == nil {
		return
	}
	reason := fmt.Sprintf("rolled back (%s); resume to sync from Git again", op.Message)
	if err := c.Store.SetPaused(app.Metadata.Name, true, reason); err != nil {
		c.Log.Error("pausing after rollback", "app", app.Metadata.Name, "error", err)
	}
}

// recoverSelf finishes the operations on composecd itself that helpers carried
// out while composecd was replaced, and abandons a pending self release whose
// helper is gone. It runs at the first refresh that can reach their hosts.
func (c *Controller) recoverSelf(ctx context.Context, objs *config.Objects) {
	c.mu.RLock()
	done := c.selfRecovered
	c.mu.RUnlock()
	if done {
		return
	}
	ops, err := c.Store.HelperOperations()
	if err != nil {
		c.Log.Error("reading operations left to helper containers", "error", err)
		return
	}
	helped := map[string]bool{}
	for _, op := range ops {
		app := objs.Application(op.App)
		var h *hosts.Host
		if app != nil {
			h = c.Hosts.Get(app.Spec.Destination.Host)
		}
		if h == nil {
			op.Phase, op.Message = store.PhaseFailed, "composecd restarted, and the Application or its host is no longer defined to find the helper container on"
			c.finishOperation(op.App, op)
			continue
		}
		if h.Status().State != hosts.StateUp {
			return // try again at the next refresh
		}
		helped[op.App] = true
		obj, err := h.Inspect(ctx, "container", op.Helper)
		if err != nil {
			return
		}
		c.mu.Lock()
		c.running[op.App] = op.ID
		c.liveLogs[op.ID] = &logBuffer{max: maxLog}
		c.mu.Unlock()
		if len(obj) == 0 {
			op.Phase, op.Message = store.PhaseFailed, fmt.Sprintf("the helper container %s is gone", op.Helper)
			c.abandonSelf(h, op.App, op.Message)
			c.finishOperation(op.App, op)
			continue
		}
		c.mu.Lock()
		c.selfUpdating = op.App
		c.mu.Unlock()
		c.Log.Info("finishing an update of composecd carried out by a helper container", "app", op.App, "helper", op.Helper)
		c.ops.Add(1)
		go func() {
			defer c.ops.Done()
			c.awaitHelper(ctx, h, op, c.opLog(op))
			c.afterSelfOperation(app, op)
			c.finishOperation(op.App, op)
			h.Invalidate()
			c.reconcileAsync(app, false)
		}()
	}
	for _, a := range objs.Applications {
		name := a.Metadata.Name
		if helped[name] || !c.isSelfApp(a) {
			continue
		}
		if h := c.Hosts.Get(a.Spec.Destination.Host); h != nil && h.Status().State == hosts.StateUp {
			c.abandonSelf(h, name, "abandoned: composecd restarted with no helper container applying it")
		}
	}
	c.mu.Lock()
	c.selfRecovered = true
	c.mu.Unlock()
}

// abandonSelf marks name's latest revision failed if it is still pending: the
// helper that would have finished it is gone.
func (c *Controller) abandonSelf(h *hosts.Host, name, reason string) {
	cfg := &action.Configuration{Store: c.Hosts.Backend.Store(h)}
	rels, err := cfg.Store.History(name)
	if err != nil || len(rels) == 0 || !rels[len(rels)-1].Status.IsPending() {
		return
	}
	rev := rels[len(rels)-1].Revision
	if err := action.Abandon(cfg, name, rev, reason); err != nil {
		c.Log.Error("abandoning a pending revision of composecd", "app", name, "revision", rev, "error", err)
		return
	}
	c.Log.Warn("abandoned a pending revision of composecd", "app", name, "revision", rev, "reason", reason)
	h.Invalidate()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
