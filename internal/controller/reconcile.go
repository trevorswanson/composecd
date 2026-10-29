package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/diff"
	"github.com/trevorswanson/composetpl/pkg/hooks"
	"github.com/trevorswanson/composetpl/pkg/output"
	"github.com/trevorswanson/composetpl/pkg/registry"
	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/source"
)

// plan is what a reconcile learned that a sync needs.
type plan struct {
	app  *config.Application
	host *hosts.Host
	prep *source.Prepared
	// key identifies the desired state, for auto-sync's retry suppression.
	key string
	// gitChanged is true when Git differs from the deployed release (not just
	// live drift).
	gitChanged bool
	// drifted is true when containers are missing, changed or orphaned compared
	// with the deployed release, which self-heal repairs.
	drifted bool
	// release is the end of the checkout's use; call it when done with prep.
	release func()
}

func (p *plan) done() {
	if p != nil && p.release != nil {
		p.release()
	}
}

// reconcile assesses app and, if auto, lets automation act on it (sync,
// self-heal), then records its status.
func (c *Controller) reconcile(ctx context.Context, app *config.Application, fetchErr error, auto bool) {
	defer c.lockApp(app.Metadata.Name)()
	st, p := c.assess(ctx, app, fetchErr)
	if auto && c.automate(ctx, app, st, p, true) {
		p.done()
		st, p = c.assess(ctx, app, nil)
		c.automate(ctx, app, st, p, false)
	}
	p.done()
	c.setStatus(st)
}

func baseStatus(app *config.Application) *AppStatus {
	return &AppStatus{
		Name:      app.Metadata.Name,
		Labels:    app.Metadata.Labels,
		Host:      app.Spec.Destination.Host,
		Source:    app.Spec.Source,
		Origin:    app.Origin,
		Automated: app.Spec.SyncPolicy.Automated != nil,
		DependsOn: app.Spec.DependsOn,
		Sync:      SyncStatus{Status: Unknown},
		Health:    HealthStatus{Status: health.Unknown},
	}
}

// assess compares app's desired state in Git with its deployed release and live
// containers. The plan is nil when the app could not be rendered.
func (c *Controller) assess(ctx context.Context, app *config.Application, fetchErr error) (*AppStatus, *plan) {
	name := app.Metadata.Name
	st := baseStatus(app)
	st.ReconciledAt = time.Now()
	if state, err := c.Store.AppState(name); err == nil {
		st.Paused, st.PausedReason = state.Paused, state.PausedReason
	}
	if op, err := c.Store.LastOperation(name); err == nil {
		st.Operation = op
	}
	var warnings []string
	if fetchErr != nil {
		warnings = append(warnings, "fetching "+sourceRepo(app)+": "+fetchErr.Error())
	}
	fail := func(err error) (*AppStatus, *plan) {
		st.Error = strings.Join(append(warnings, err.Error()), "; ")
		return st, nil
	}

	h := c.Hosts.Get(app.Spec.Destination.Host)
	if h == nil {
		return fail(fmt.Errorf("host %q is not defined", app.Spec.Destination.Host))
	}
	if hs := h.Status(); hs.State == hosts.StateDown {
		return fail(fmt.Errorf("host %q is down: %s", h.Name(), hs.Error))
	}
	defer h.Use()()

	cfg := h.Configuration(source.Env(h.Spec, app), io.Discard)
	// Prepare first: it learns the secret paths that reading history masks.
	p, err := c.prepare(ctx, app, h)
	hist, herr := c.releaseHistory(h, name)
	if err != nil {
		return fail(err)
	}
	if herr != nil {
		p.done()
		return fail(fmt.Errorf("reading release history: %w", herr))
	}
	st.Sync.Revision = p.prep.Revision
	st.Sync.ValuesRevision = p.prep.ValuesRevision
	if app.Spec.Source.IsOCI() {
		st.Sync.ChartRef = p.prep.ChartRef
	}
	var deployed, latest *release.Release
	if len(hist) > 0 {
		latest = hist[len(hist)-1]
		st.Release = releaseInfo(latest)
		if d, err := storage.LastDeployed(cfg.Store, name); err == nil {
			deployed = d
		}
	}

	// Render Git as it would be deployed at the deployed revision's number, so
	// charts using .Release.Revision don't look out of sync after every sync.
	revision := 1
	if deployed != nil {
		revision = deployed.Revision
	}
	manifest, hks, err := c.render(ctx, cfg, h, app, p.prep, revision)
	if err != nil {
		p.done()
		return fail(p.prep.RelativeErr(err))
	}
	projectDir := h.ProjectDir(name)
	var revisions []string
	if app.Spec.Source.IsOCI() {
		revisions = append(revisions, p.prep.ChartRef, p.prep.ValuesRevision)
	}
	owner := app.Metadata.Labels[LabelApplicationSet]
	prune := app.Spec.SyncPolicy.Automated != nil && app.Spec.SyncPolicy.Automated.Prune
	if owner != "" {
		revisions = append(revisions, owner, fmt.Sprint(prune))
	}
	p.key = desiredKey(manifest, hks, projectDir, revisions...)

	var reasons []string
	if deployed == nil {
		reasons = append(reasons, "not deployed")
	} else {
		if deployed.Labels[LabelManagedBy] != ManagedBy || deployed.Labels[LabelApp] != name {
			reasons = append(reasons, "release is not yet managed by this Application")
		}
		if owner != deployed.Labels[LabelApplicationSet] {
			reasons = append(reasons, "ApplicationSet ownership changed")
		}
		if (owner != "" || deployed.Labels[LabelApplicationSet] != "") && (deployed.Labels[LabelPrune] == "true") != prune {
			reasons = append(reasons, "ApplicationSet prune policy changed")
		}
		st.Sync.DeployedRevision = deployed.Labels[LabelSourceRevision]
		st.Sync.DeployedValuesRevision = deployed.Labels[LabelValuesRevision]
		if st.Sync.ValuesRevision != st.Sync.DeployedValuesRevision {
			reasons = append(reasons, "values revision differs from Git")
		}
		if app.Spec.Source.IsOCI() && deployed.Chart.Ref != p.prep.ChartRef {
			reasons = append(reasons, "chart reference differs from the desired OCI digest")
		}
		changes, err := diff.Manifests(deployed.Manifest, manifest)
		if err != nil {
			p.done()
			return fail(err)
		}
		for _, ch := range changes {
			st.Sync.Changes = append(st.Sync.Changes, toChange(name, ch))
		}
		if len(changes) > 0 {
			reasons = append(reasons, fmt.Sprintf("%d %s differ from Git", len(changes), plural(len(changes), "resource", "resources")))
		}
		if !sameHooks(deployed.Hooks, hks) {
			reasons = append(reasons, "hooks differ from Git")
		}
		if deployed.ProjectDir != projectDir {
			reasons = append(reasons, fmt.Sprintf("project directory changes from %s to %s", deployed.ProjectDir, projectDir))
		}
	}
	if latest != nil && latest.Status == release.StatusFailed && (deployed == nil || latest.Revision > deployed.Revision) {
		warnings = append(warnings, fmt.Sprintf("revision %d failed: %s", latest.Revision, latest.Description))
	}
	p.gitChanged = len(reasons) > 0

	containers, err := cfg.Compose.PS(ctx, name)
	if err != nil {
		warnings = append(warnings, "reading containers: "+err.Error())
	}
	st.Health.Containers = containers
	if err == nil {
		st.Health.Status, st.Health.Message = health.Assess(deployed != nil, containers)
	}
	if deployed != nil {
		drift, err := action.NewRepair(cfg).Drift(ctx, name)
		switch {
		case err != nil:
			warnings = append(warnings, "checking drift: "+err.Error())
		case len(drift) > 0:
			st.Sync.Drift = drift
			var parts []string
			for _, d := range drift {
				// A stopped container is health's business, not a difference from Git.
				if d.Reason != action.DriftStopped {
					parts = append(parts, d.Service+" "+d.Reason)
				}
			}
			if len(parts) > 0 {
				reasons = append(reasons, "live containers drifted: "+strings.Join(parts, ", "))
				p.drifted = true
			}
		}
	}

	st.Sync.Reasons = reasons
	if len(reasons) > 0 {
		st.Sync.Status = OutOfSync
	} else {
		st.Sync.Status = Synced
	}
	st.Error = strings.Join(warnings, "; ")
	return st, p
}

// prepare resolves app's source revision and checkout.
func (c *Controller) prepare(ctx context.Context, app *config.Application, h *hosts.Host) (*plan, error) {
	s := app.Spec.Source
	p := &plan{app: app, host: h}
	var checkout, sha string
	url, revision := s.GitRepository()
	if url != "" {
		var err error
		if sha, err = c.Git.Resolve(ctx, url, revision); err != nil {
			return nil, err
		}
		p.release = c.useCheckout(url, sha)
		if checkout, err = c.Git.Checkout(ctx, url, sha); err != nil {
			p.done()
			return nil, err
		}
	}
	prep, err := source.Prepare(ctx, app, h.Spec, checkout, sha, c.Registry, c.Boot.SOPSAgeKeyFile)
	if prep != nil {
		c.rememberPaths(app.Metadata.Name, prep.SecretPaths)
	}
	if err != nil {
		p.done()
		return nil, err
	}
	if s.IsOCI() {
		_, ref, err := action.LoadChart(ctx, c.Registry, s.Chart, s.Version)
		if err != nil {
			p.done()
			return nil, fmt.Errorf("resolving OCI chart: %w", err)
		}
		// Normalize to the same digest-only form composetpl records after
		// loading a reference containing both a tag and a digest.
		pinned, err := registry.ParseReference(ref)
		if err != nil {
			p.done()
			return nil, err
		}
		prep.ChartRef, prep.Version = pinned.String(), ""
	}
	c.remember(app.Metadata.Name, prep.Secrets)
	p.prep = prep
	return p, nil
}

// render renders app's chart the way composetpl's upgrade does, as revision
// revision, and returns the merged manifest (hooks removed) and the hooks.
// lookup reads the live engine, as it does during a real sync.
func (c *Controller) render(ctx context.Context, cfg *action.Configuration, h *hosts.Host, app *config.Application, prep *source.Prepared, revision int) (string, []*release.Hook, error) {
	caps, err := capabilities(ctx, cfg, h)
	if err != nil {
		return "", nil, err
	}
	r, err := action.Render(action.RenderOptions{
		ChartPath: prep.ChartRef, Version: prep.Version, Registry: c.Registry, Context: ctx,
		ReleaseName: app.Metadata.Name, Revision: revision, IsUpgrade: revision > 1,
		UserValues: prep.Values, Env: prep.Lookup, Capabilities: caps,
		Lookup: func(kind, name string) (map[string]interface{}, error) {
			return cfg.Compose.Inspect(ctx, kind, name)
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("rendering: %w", err)
	}
	root, err := r.Merge()
	if err != nil {
		return "", nil, fmt.Errorf("rendering: %w", err)
	}
	hks, err := hooks.Extract(root)
	if err != nil {
		return "", nil, fmt.Errorf("rendering: %w", err)
	}
	output.SetName(root, app.Metadata.Name)
	manifest, err := output.Encode(root)
	if err != nil {
		return "", nil, err
	}
	return string(manifest), hks, nil
}

// capabilities is .Capabilities for h, from its last ping when it has one.
func capabilities(ctx context.Context, cfg *action.Configuration, h *hosts.Host) (map[string]interface{}, error) {
	s := h.Status()
	cv, dv := s.ComposeVersion, s.DockerVersion
	if cv == "" || dv == "" {
		var err error
		if cv, err = cfg.Compose.Version(ctx); err != nil {
			return nil, err
		}
		if dv, err = cfg.Compose.ServerVersion(ctx); err != nil {
			return nil, err
		}
	}
	return map[string]interface{}{"ComposeVersion": cv, "DockerVersion": dv}, nil
}

func desiredKey(manifest string, hks []*release.Hook, projectDir string, revisions ...string) string {
	h := sha256.New()
	hj, _ := json.Marshal(hks)
	parts := [][]byte{[]byte(manifest), hj, []byte(projectDir)}
	for _, r := range revisions {
		parts = append(parts, []byte(r))
	}
	for _, part := range parts {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sameHooks(a, b []*release.Hook) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return bytes.Equal(aj, bj)
}

func toChange(name string, ch diff.Change) Change {
	var buf bytes.Buffer
	_ = diff.Print(&buf, []diff.Change{ch}, diff.PrintOptions{Release: name, Context: 3})
	// Drop Print's header line; Resource and Op carry it.
	text := buf.String()
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return Change{Resource: ch.Resource, Op: string(ch.Op), Diff: strings.TrimRight(text, "\n") + "\n"}
}

func releaseInfo(r *release.Release) *Release {
	labels := map[string]string{}
	keys := make([]string, 0, len(r.Labels))
	for k := range r.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labels[k] = r.Labels[k]
	}
	return &Release{
		Revision: r.Revision, Status: string(r.Status), Description: r.Description,
		Chart: r.Chart.String(), ChartRef: r.Chart.Ref, AppVersion: r.Chart.AppVersion, LastDeployed: r.LastDeployed, Labels: labels,
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
