package controller

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/compose/composetest"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

type fakeBackend struct {
	dir    string
	fakes  map[string]*composetest.Fake
	mu     sync.Mutex
	envs   map[string][]string
	events chan string // container events for host "nas"
}

func (b *fakeBackend) Watch(ctx context.Context, h *hosts.Host, on func(string)) error {
	if h.Name() != "nas" {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p := <-b.events:
			on(p)
		}
	}
}

func (b *fakeBackend) Runner(h *hosts.Host, env []string, _ io.Writer) compose.Runner {
	b.mu.Lock()
	b.envs[h.Name()] = env
	b.mu.Unlock()
	return b.fakes[h.Name()]
}

func (b *fakeBackend) Store(h *hosts.Host) storage.Driver {
	return storage.NewLocal(filepath.Join(b.dir, h.Name()))
}

type env struct {
	t       *testing.T
	repo    string
	c       *Controller
	backend *fakeBackend
}

const hostsYAML = `apiVersion: composecd/v1alpha1
kind: Host
metadata: {name: nas}
spec: {projectRoot: /srv, env: {TZ: UTC}}
`

const chartYAML = "apiVersion: v2\nname: web\nversion: 0.1.0\nappVersion: '1'\n"

// webTemplate uses .Release.Revision so the tests catch a sync loop.
const webTemplate = `services:
  web:
    image: nginx:{{ .Values.tag }}
    labels:
      revision: "{{ .Release.Revision }}"
      tz: {{ env "TZ" | quote }}
`

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, repo: t.TempDir()}
	e.git("init", "--quiet", "--initial-branch", "main")
	e.commit(map[string]string{
		"composecd/hosts.yaml":        hostsYAML,
		"composecd/apps.yaml":         appYAML("web", true, "") + "---\n" + appYAML("manual", false, ""),
		"charts/web/Chart.yaml":       chartYAML,
		"charts/web/values.yaml":      "tag: '1'\n",
		"charts/web/templates/a.yaml": webTemplate,
	})
	dir := t.TempDir()
	e.backend = &fakeBackend{dir: filepath.Join(dir, "releases"), fakes: map[string]*composetest.Fake{"nas": composetest.New()},
		envs: map[string][]string{}, events: make(chan string)}
	st, err := store.Open(filepath.Join(dir, "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	boot := &config.Bootstrap{
		Concurrency: 2, RefreshInterval: config.Duration(config.DefaultRefreshInterval),
		Root: config.Root{RepoURL: e.repo, TargetRevision: "main", Path: "composecd"},
	}
	e.c = New(Options{
		Boot: boot, Git: gitcache.New(filepath.Join(dir, "git"), nil), Hosts: hosts.NewRegistry(e.backend),
		Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Stop watchers and timers, and let background operations finish, before
	// the temp dirs are removed.
	t.Cleanup(func() {
		e.c.Close()
		e.c.Wait()
	})
	return e
}

func appYAML(name string, automated bool, extra string) string {
	s := "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n" +
		"  source: {path: charts/web" + extra + "}\n  destination: {host: nas}\n"
	if automated {
		s += "  syncPolicy: {automated: {}, options: {wait: false}}\n"
	}
	return s
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1", "HOME="+e.repo)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) commit(files map[string]string) string {
	e.t.Helper()
	for name, content := range files {
		p := filepath.Join(e.repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	e.git("add", "--all")
	e.git("commit", "--quiet", "-m", "update")
	return e.git("rev-parse", "HEAD")
}

func (e *env) fake() *composetest.Fake { return e.backend.fakes["nas"] }

func (e *env) refresh() {
	e.t.Helper()
	e.c.RefreshAll(context.Background())
	e.c.ops.Wait()
}

func (e *env) app(name string) AppStatus {
	e.t.Helper()
	st, err := e.c.App(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) ups(name string) int {
	return strings.Count(e.fake().CallLog()+";", "up "+name+" ")
}

func TestAutoSyncAndUpgrade(t *testing.T) {
	e := newEnv(t)
	first := e.git("rev-parse", "HEAD")
	e.refresh()

	web := e.app("web")
	if web.Sync.Status != Synced || web.Health.Status != health.Healthy || web.Error != "" {
		t.Fatalf("web after first refresh: %+v", web)
	}
	if web.Sync.Revision != first || web.Sync.DeployedRevision != first || web.Release.Revision != 1 {
		t.Errorf("revisions: %+v release %+v", web.Sync, web.Release)
	}
	if l := web.Release.Labels; l[LabelManagedBy] != ManagedBy || l[LabelApp] != "web" || l[LabelOperationID] == "" {
		t.Errorf("labels = %v", l)
	}
	if web.Operation == nil || web.Operation.Phase != store.PhaseSucceeded || web.Operation.Trigger != store.TriggerAuto {
		t.Errorf("operation = %+v", web.Operation)
	}
	running := e.fake().Running["web"]
	if !strings.Contains(running, "nginx:1") || !strings.Contains(running, `tz: "UTC"`) {
		t.Errorf("deployed manifest:\n%s", running)
	}
	if !contains(e.backend.envs["nas"], "TZ=UTC") {
		t.Errorf("docker env = %v", e.backend.envs["nas"])
	}

	// The manual app is left alone.
	manual := e.app("manual")
	if manual.Sync.Status != OutOfSync || manual.Sync.Reasons[0] != "not deployed" || manual.Health.Status != health.Missing {
		t.Errorf("manual: %+v", manual)
	}
	if e.ups("manual") != 0 {
		t.Error("manual app was synced automatically")
	}

	// Nothing changed: no new sync, even though the chart uses .Release.Revision.
	e.refresh()
	if n := e.ups("web"); n != 1 {
		t.Errorf("web was synced %d times without changes", n)
	}
	if e.app("web").Sync.Status != Synced {
		t.Errorf("web not synced: %+v", e.app("web").Sync)
	}

	second := e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	web = e.app("web")
	if web.Sync.Status != Synced || web.Release.Revision != 2 || web.Sync.DeployedRevision != second {
		t.Errorf("after upgrade: %+v release %+v", web.Sync, web.Release)
	}
	if !strings.Contains(e.fake().Running["web"], "nginx:2") {
		t.Errorf("not upgraded:\n%s", e.fake().Running["web"])
	}
	hist, err := e.c.History("web")
	if err != nil || len(hist) != 2 || hist[0].Status != "superseded" || hist[1].Description != "composecd sync to "+second[:7] {
		t.Errorf("history = %+v, %v", hist, err)
	}
	ops, _ := e.c.Operations("web", 10)
	if len(ops) != 2 || ops[0].ReleaseRevision != 2 || ops[0].SourceRevision != second {
		t.Errorf("operations = %+v", ops)
	}
}

func TestManualSyncAndDiff(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	manual := e.app("manual")
	// An undeployed app diffs against nothing: every resource is added.
	if ch := manual.Sync.Changes; len(ch) != 2 || ch[0].Op != "added" || ch[1].Op != "added" || len(manual.Sync.Drift) != 0 {
		t.Errorf("undeployed changes = %+v, drift = %+v", ch, manual.Sync.Drift)
	}

	op, err := e.c.Sync("manual", "trevor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Sync("manual", "trevor"); err != ErrBusy && err != nil {
		t.Errorf("second sync: %v", err)
	}
	e.c.ops.Wait()
	got, err := e.c.Operation(op.ID)
	if err != nil || got.Phase != store.PhaseSucceeded || got.Trigger != store.TriggerManual || got.ReleaseRevision != 1 || got.User != "trevor" {
		t.Fatalf("operation = %+v, %v", got, err)
	}
	if e.app("manual").Sync.Status != Synced {
		t.Errorf("manual after sync: %+v", e.app("manual").Sync)
	}

	// A Git change shows as a diff, not synced automatically.
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '3'\n"})
	e.refresh()
	manual = e.app("manual")
	if manual.Sync.Status != OutOfSync || len(manual.Sync.Changes) != 1 {
		t.Fatalf("manual after change: %+v", manual.Sync)
	}
	ch := manual.Sync.Changes[0]
	if ch.Resource != "services/web" || ch.Op != "changed" || !strings.Contains(ch.Diff, "-image: nginx:1") || !strings.Contains(ch.Diff, "+image: nginx:3") {
		t.Errorf("change = %+v", ch)
	}
	if _, err := e.c.Sync("nope", "trevor"); err == nil {
		t.Error("sync of an unknown app should fail")
	}
}

func TestFailedAutoSyncIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.fake().FailUp = "nginx:1"
	e.refresh()
	web := e.app("web")
	if web.Operation == nil || web.Operation.Phase != store.PhaseFailed || !strings.Contains(web.Operation.Message, "unhealthy") {
		t.Fatalf("operation = %+v", web.Operation)
	}
	if web.Sync.Status != OutOfSync || !strings.Contains(web.Error, "revision 1 failed") {
		t.Errorf("status = %+v error %q", web.Sync, web.Error)
	}
	e.refresh()
	if n := e.ups("web"); n != 1 {
		t.Errorf("failed sync was retried: %d ups", n)
	}
	// A fix in Git is tried.
	e.fake().FailUp = ""
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	if web := e.app("web"); web.Sync.Status != Synced || e.ups("web") != 2 {
		t.Errorf("after fix: %+v (%d ups)", web.Sync, e.ups("web"))
	}
}

func TestRollbackPausesAndResume(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()

	op, err := e.c.Rollback("web", 1, "trevor")
	if err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	if got, _ := e.c.Operation(op.ID); got.Phase != store.PhaseSucceeded || got.ReleaseRevision != 3 {
		t.Fatalf("rollback = %+v", got)
	}
	web := e.app("web")
	if !web.Paused || !strings.Contains(web.PausedReason, "Rollback to 1") || web.Sync.Status != OutOfSync {
		t.Errorf("after rollback: paused %v %q sync %+v", web.Paused, web.PausedReason, web.Sync)
	}
	if !strings.Contains(e.fake().Running["web"], "nginx:1") {
		t.Error("rollback did not redeploy revision 1")
	}
	e.refresh()
	if strings.Contains(e.fake().Running["web"], "nginx:2") {
		t.Error("paused app was synced")
	}

	if err := e.c.Resume("web"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	if web := e.app("web"); web.Paused || web.Sync.Status != Synced || !strings.Contains(e.fake().Running["web"], "nginx:2") {
		t.Errorf("after resume: %+v", web)
	}
	if err := e.c.Pause("web", ""); err != nil {
		t.Fatal(err)
	}
	if web := e.app("web"); !web.Paused || web.PausedReason != "paused" {
		t.Errorf("pause: %+v", web)
	}
}

func TestDriftIsReportedNotSynced(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	e.fake().Running["web"] = "services: {web: {image: hacked}}\n"
	e.refresh()
	web := e.app("web")
	if web.Sync.Status != OutOfSync || len(web.Sync.Drift) != 1 || web.Sync.Drift[0].Reason != "changed" {
		t.Errorf("drift: %+v", web.Sync)
	}
	if !strings.Contains(strings.Join(web.Sync.Reasons, ";"), "live containers drifted: web changed") {
		t.Errorf("reasons = %v", web.Sync.Reasons)
	}
	if e.ups("web") != 1 {
		t.Error("drift alone should not trigger an upgrade before self-heal exists")
	}
}

func TestBadObjectsKeepTheLastGoodSet(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	e.commit(map[string]string{"composecd/apps.yaml": "apiVersion: composecd/v1alpha1\nkind: Nope\n"})
	e.refresh()
	root := e.c.Root()
	if !strings.Contains(root.Error, "unknown kind") {
		t.Errorf("root error = %q", root.Error)
	}
	if len(e.c.Apps()) != 2 {
		t.Errorf("apps = %d, want the last good 2", len(e.c.Apps()))
	}
	// Removing an app from Git drops it (the release stays until prune exists).
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("web", true, "")})
	e.refresh()
	if apps := e.c.Apps(); len(apps) != 1 || apps[0].Name != "web" {
		t.Errorf("apps = %+v", apps)
	}
	if _, err := e.c.App("manual"); err == nil {
		t.Error("removed app still served")
	}
	if hs := e.c.HostStatuses(); len(hs) != 1 || hs[0].State != hosts.StateUp || hs[0].Apps != 1 || hs[0].DockerVersion != "29.0.0" {
		t.Errorf("hosts = %+v", hs)
	}
}

func TestRenderErrorIsReported(t *testing.T) {
	e := newEnv(t)
	e.commit(map[string]string{"charts/web/templates/a.yaml": "services: {{ fail \"boom\" }}\n"})
	e.refresh()
	web := e.app("web")
	if web.Sync.Status != Unknown || !strings.Contains(web.Error, "boom") {
		t.Errorf("web = %+v", web)
	}
	if e.ups("web") != 0 {
		t.Error("broken chart was deployed")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (b *fakeBackend) Logs(context.Context, *hosts.Host, string, int, bool, io.Writer) error {
	return nil
}

func TestUndeployedDiff(t *testing.T) {
	e := newEnv(t)
	// A stack started by hand under the app's project name, before any sync.
	e.fake().Containers = map[string][]compose.Container{"manual": {
		{Name: "manual-web-1", Service: "web", State: "running", Labels: compose.Labels{compose.ConfigHashLabel: "stale"}},
		{Name: "manual-db-1", Service: "db", State: "running"},
	}}
	e.refresh()

	manual := e.app("manual")
	if len(manual.Sync.Changes) != 2 || manual.Sync.Changes[0].Resource != "name" || manual.Sync.Changes[1].Resource != "services/web" {
		t.Fatalf("changes = %+v", manual.Sync.Changes)
	}
	if ch := manual.Sync.Changes[1]; ch.Op != "added" || !strings.Contains(ch.Diff, "+image: nginx:1") {
		t.Errorf("web change = %+v", ch)
	}
	drift := manual.Sync.Drift
	if len(drift) != 2 || drift[0].Service != "db" || drift[0].Reason != "orphaned" || drift[1].Service != "web" || drift[1].Reason != "changed" {
		t.Errorf("drift = %+v", drift)
	}
	if got := strings.Join(manual.Sync.Reasons, ";"); got != "not deployed;existing containers differ from Git: db orphaned, web changed" {
		t.Errorf("reasons = %q", got)
	}
	if manual.Error != "" {
		t.Errorf("error = %q", manual.Error)
	}
}
