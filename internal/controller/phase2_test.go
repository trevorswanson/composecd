package controller

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/compose/composetest"
	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Notify(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) types() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Type+":"+e.App)
	}
	return strings.Join(out, " ")
}

// app writes one Application with a raw spec tail (dependsOn, syncPolicy, ...).
func app(name, host, tail string) string {
	return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n" +
		"  source: {path: charts/web}\n  destination: {host: " + host + "}\n" + tail + "---\n"
}

func (e *env) apps(docs ...string) {
	e.t.Helper()
	e.commit(map[string]string{"composecd/apps.yaml": strings.Join(docs, "")})
}

// eventually polls cond (for work done by retry and heal timers).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSelfHeal(t *testing.T) {
	base := healBackoffBase
	healBackoffBase = 200 * time.Millisecond
	t.Cleanup(func() { healBackoffBase = base })
	e := newEnv(t)
	rec := &recorder{}
	e.c.Notify = rec
	e.apps(app("web", "nas", "  syncPolicy: {automated: {selfHeal: true}, options: {wait: false}}\n"))
	e.refresh()
	deployed := e.fake().Running["web"]

	e.fake().Running["web"] = "services: {web: {image: hacked}}\n"
	e.refresh()
	if e.fake().Running["web"] != deployed {
		t.Fatalf("not healed:\n%s", e.fake().Running["web"])
	}
	web := e.app("web")
	if web.Sync.Status != Synced || web.Release.Revision != 1 || web.Operation.Kind != store.KindHeal ||
		!strings.Contains(web.Operation.Message, "repaired web changed to revision 1") {
		t.Errorf("after heal: %+v op %+v", web.Sync, web.Operation)
	}
	if !strings.Contains(rec.types(), "self-healed:web") || !strings.HasPrefix(rec.types(), "sync-succeeded:web") {
		t.Errorf("events = %s", rec.types())
	}

	// Drifting again right away hits the backoff.
	e.fake().Running["web"] = "services: {web: {image: hacked-again}}\n"
	e.refresh()
	web = e.app("web")
	if !strings.Contains(web.Blocked, "self-heal backing off") || web.Sync.Status != OutOfSync {
		t.Errorf("backoff: blocked %q sync %+v", web.Blocked, web.Sync)
	}
	// The scheduled retry heals it once the backoff passes.
	eventually(t, "the second heal", func() bool {
		e.c.Wait()
		return e.app("web").Sync.Status == Synced
	})
}

func TestEventTriggersHeal(t *testing.T) {
	debounce := eventDebounce
	eventDebounce = 10 * time.Millisecond
	t.Cleanup(func() { eventDebounce = debounce })
	e := newEnv(t)
	e.apps(app("web", "nas", "  syncPolicy: {automated: {selfHeal: true}, options: {wait: false}}\n"))
	e.refresh()
	deployed := e.fake().Running["web"]

	// No refresh: the container event alone gets the app reconciled and healed.
	e.fake().Running["web"] = "services: {web: {image: hacked}}\n"
	e.backend.events <- "unknown-project"
	e.backend.events <- "web"
	eventually(t, "the event-driven heal", func() bool {
		e.c.Wait()
		return e.app("web").Operation.Kind == store.KindHeal
	})
	if e.fake().Running["web"] != deployed {
		t.Errorf("not healed:\n%s", e.fake().Running["web"])
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	e.c.Notify = rec
	e.apps(
		app("web", "nas", "  syncPolicy: {automated: {prune: true}, options: {wait: false}}\n"),
		app("keep", "nas", "  syncPolicy: {automated: {}, options: {wait: false}}\n"),
		app("other", "nas", "  syncPolicy: {automated: {prune: true}, options: {wait: false}}\n"),
	)
	e.refresh()
	if len(e.fake().Running) != 3 {
		t.Fatalf("running = %v", e.fake().Running)
	}
	if rel, _ := e.backend.Store(e.c.Hosts.Get("nas")).Get("web", 1); rel.Labels[LabelPrune] != "true" {
		t.Errorf("labels = %v", rel.Labels)
	}

	// Removing every app looks like a broken root: nothing is pruned.
	e.commit(map[string]string{"composecd/apps.yaml": "# nothing\n"})
	e.refresh()
	if len(e.fake().Running) != 3 {
		t.Errorf("an empty root pruned: %v", e.fake().Running)
	}

	e.apps(app("other", "nas", "  syncPolicy: {automated: {prune: true}, options: {wait: false}}\n"))
	e.refresh()
	if _, ok := e.fake().Running["web"]; ok {
		t.Error("web was not pruned")
	}
	if _, ok := e.fake().Running["keep"]; !ok {
		t.Error("keep (prune off) was pruned")
	}
	h := e.c.HostStatuses()[0]
	if strings.Join(h.Orphans, ",") != "keep" {
		t.Errorf("orphans = %v", h.Orphans)
	}
	hist, err := e.backend.Store(e.c.Hosts.Get("nas")).History("web")
	if err != nil || hist[len(hist)-1].Status != release.StatusUninstalled {
		t.Errorf("web history kept as uninstalled: %v %v", hist, err)
	}
	ops, _ := e.c.Store.Operations("web", 1)
	if len(ops) != 1 || ops[0].Kind != store.KindPrune || ops[0].Phase != store.PhaseSucceeded {
		t.Errorf("prune operation = %+v", ops)
	}
	if !strings.Contains(rec.types(), "prune:web") {
		t.Errorf("events = %s", rec.types())
	}
	// Pruned once only.
	e.refresh()
	if ops, _ := e.c.Store.Operations("web", 5); len(ops) != 2 {
		t.Errorf("web operations = %d, want sync and one prune", len(ops))
	}

	// An orphan can be deleted by hand; an app still in Git can't.
	if _, err := e.c.DeleteRelease("nas", "other", "admin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an app in Git: %v", err)
	}
	op, err := e.c.DeleteRelease("nas", "keep", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if _, ok := e.fake().Running["keep"]; ok {
		t.Error("keep was not deleted")
	}
	if got, _ := e.c.Store.Operation(op.ID); got.Phase != store.PhaseSucceeded || got.Trigger != store.TriggerManual || got.User != "admin" {
		t.Errorf("delete operation = %+v", got)
	}
	if h := e.c.HostStatuses()[0]; len(h.Orphans) != 0 {
		t.Errorf("orphans after delete = %v", h.Orphans)
	}
}

func TestBrokenDocumentDoesNotPrune(t *testing.T) {
	e := newEnv(t)
	policy := "  syncPolicy: {automated: {prune: true}, options: {wait: false}}\n"
	e.apps(app("web", "nas", policy), app("other", "nas", policy), app("keep", "nas", policy))
	e.refresh()
	if len(e.fake().Running) != 3 {
		t.Fatalf("running = %v", e.fake().Running)
	}
	// Dropping the --- between web and other merges them into one document with
	// duplicate keys. That must be an error, not two apps vanishing (and pruned).
	merged := strings.Replace(app("web", "nas", policy), "---\n", "", 1) + app("other", "nas", policy) + app("keep", "nas", policy)
	e.apps(merged)
	e.refresh()
	if err := e.c.Root().Error; !strings.Contains(err, "already defined") {
		t.Errorf("root error = %q", err)
	}
	if len(e.fake().Running) != 3 {
		t.Errorf("apps were pruned after a broken commit: %v", e.fake().Running)
	}
	if len(e.c.Apps()) != 3 {
		t.Errorf("apps = %d, want the last good 3", len(e.c.Apps()))
	}
}

func TestMoveToAnotherHost(t *testing.T) {
	e := newEnv(t)
	e.backend.fakes["pi"] = composetest.New()
	hostsDoc := hostsYAML + "---\napiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: pi}\nspec: {url: 'ssh://pi'}\n"
	policy := "  syncPolicy: {automated: {prune: true}, options: {wait: false}}\n"
	e.commit(map[string]string{"composecd/hosts.yaml": hostsDoc, "composecd/apps.yaml": app("web", "nas", policy)})
	e.refresh()
	e.apps(app("web", "pi", policy))
	e.refresh()
	if _, ok := e.backend.fakes["pi"].Running["web"]; !ok {
		t.Error("web not deployed on pi")
	}
	if _, ok := e.fake().Running["web"]; ok {
		t.Error("web not pruned from nas")
	}
	if web := e.app("web"); web.Host != "pi" || web.Sync.Status != Synced {
		t.Errorf("web = %+v", web)
	}
}

func TestRetry(t *testing.T) {
	e := newEnv(t)
	e.fake().FailUp = "nginx:1"
	e.apps(app("web", "nas", "  syncPolicy:\n    automated: {}\n    options: {wait: false}\n    retry: {limit: 2, backoff: {duration: 50ms, maxDuration: 100ms}}\n"))
	e.refresh()
	web := e.app("web")
	if !strings.Contains(web.Blocked, "retrying in") || web.Retry == nil || web.Retry.Failures != 1 || web.Retry.Limit != 2 {
		t.Fatalf("after the first failure: blocked %q retry %+v", web.Blocked, web.Retry)
	}
	eventually(t, "two retries", func() bool {
		e.c.Wait()
		return e.ups("web") == 3 && strings.Contains(e.app("web").Blocked, "last sync of this revision failed")
	})
	time.Sleep(300 * time.Millisecond)
	e.c.Wait()
	if n := e.ups("web"); n != 3 {
		t.Errorf("ups = %d, want 1 + 2 retries", n)
	}
	if r := e.app("web").Retry; r == nil || r.Failures != 3 || !r.NextAt.IsZero() {
		t.Errorf("retry = %+v", r)
	}
}

func TestSyncWindows(t *testing.T) {
	e := newEnv(t)
	deny := func(manual bool) string {
		m := "false"
		if manual {
			m = "true"
		}
		return "apiVersion: composecd/v1alpha1\nkind: SyncWindow\nmetadata: {name: freeze}\nspec: {kind: deny, schedule: '* * * * *', duration: 2h, applications: ['*'], manualSync: " + m + "}\n---\n"
	}
	policy := "  syncPolicy: {automated: {}, options: {wait: false}}\n"
	e.apps(deny(false), app("web", "nas", policy))
	e.refresh()
	web := e.app("web")
	if e.ups("web") != 0 || !strings.Contains(web.Blocked, "deny window freeze") {
		t.Errorf("deny window: %d ups, blocked %q", e.ups("web"), web.Blocked)
	}
	if _, err := e.c.Sync("web", "trevor"); err == nil || !strings.Contains(err.Error(), "blocked by a sync window") {
		t.Errorf("manual sync in a deny window: %v", err)
	}
	if ws := e.c.Windows(); len(ws) != 1 || !ws[0].Open || ws[0].Applications[0] != "web" {
		t.Errorf("windows = %+v", ws)
	}

	e.apps(deny(true), app("web", "nas", policy))
	e.refresh()
	if e.ups("web") != 0 {
		t.Error("manualSync windows still block automatic syncs")
	}
	if _, err := e.c.Sync("web", "trevor"); err != nil {
		t.Fatalf("manual sync with manualSync: %v", err)
	}
	e.c.Wait()
	if e.ups("web") != 1 {
		t.Error("manual sync didn't run")
	}

	// An allow window that is closed now blocks too.
	e.apps("apiVersion: composecd/v1alpha1\nkind: SyncWindow\nmetadata: {name: night}\nspec: {kind: allow, schedule: '0 3 1 1 *', duration: 1m, hosts: [nas]}\n---\n",
		app("web", "nas", policy))
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	if web := e.app("web"); !strings.Contains(web.Blocked, "outside allow windows; night opens") || e.ups("web") != 1 {
		t.Errorf("allow window: blocked %q, %d ups", web.Blocked, e.ups("web"))
	}
}

func TestDependsOn(t *testing.T) {
	e := newEnv(t)
	e.apps(
		app("db", "nas", ""),
		app("web", "nas", "  dependsOn: [db]\n  syncPolicy: {automated: {}, options: {wait: false}}\n"),
	)
	e.refresh()
	if web := e.app("web"); e.ups("web") != 0 || web.Blocked != "waiting for dependency db (OutOfSync, Missing)" {
		t.Errorf("web before db: %d ups, blocked %q", e.ups("web"), web.Blocked)
	}
	if _, err := e.c.Sync("db", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	e.refresh()
	if web := e.app("web"); e.ups("web") != 1 || web.Sync.Status != Synced || web.Blocked != "" {
		t.Errorf("web after db: %d ups, %+v", e.ups("web"), web)
	}

	// Dependencies synced in the same refresh unblock dependents in that refresh.
	f := newEnv(t)
	policy := "  syncPolicy: {automated: {}, options: {wait: false}}\n"
	f.apps(app("c", "nas", "  dependsOn: [b]\n"+policy), app("b", "nas", "  dependsOn: [a]\n"+policy), app("a", "nas", policy))
	f.refresh()
	for _, n := range []string{"a", "b", "c"} {
		if st := f.app(n); st.Sync.Status != Synced {
			t.Errorf("%s after one refresh: %+v blocked %q", n, st.Sync, st.Blocked)
		}
	}
}

func TestMaintenance(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	if err := e.c.SetMaintenance("nas", true, "disk swap"); err != nil {
		t.Fatal(err)
	}
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	if web := e.app("web"); web.Blocked != "host nas is in maintenance (disk swap)" || e.ups("web") != 1 {
		t.Errorf("in maintenance: %q, %d ups", web.Blocked, e.ups("web"))
	}
	if hs := e.c.HostStatuses()[0]; !hs.Maintenance || hs.MaintenanceReason != "disk swap" {
		t.Errorf("host status = %+v", hs)
	}
	if err := e.c.SetMaintenance("nas", false, ""); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if web := e.app("web"); web.Sync.Status != Synced || e.ups("web") != 2 {
		t.Errorf("after maintenance: %+v, %d ups", web.Sync, e.ups("web"))
	}
	if err := e.c.SetMaintenance("nope", true, ""); err == nil {
		t.Error("unknown host accepted")
	}
}

func TestDegradedNotification(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	e.c.Notify = rec
	e.refresh()
	e.fake().Containers = map[string][]compose.Container{"web": {{Name: "web-web-1", Service: "web", State: "exited", ExitCode: 1,
		Labels: compose.Labels{compose.ConfigHashLabel: composetest.Hash(e.fake().Running["web"])}}}}
	e.refresh()
	if web := e.app("web"); web.Health.Status != health.Degraded {
		t.Fatalf("health = %+v", web.Health)
	}
	e.refresh()
	if got := strings.Count(rec.types(), "health-degraded:web"); got != 1 {
		t.Errorf("degraded events = %d (%s)", got, rec.types())
	}
	m := e.c.OperationMetrics()
	if len(m) != 1 || m[0] != (OperationMetric{Kind: "sync", Trigger: "auto", Phase: "Succeeded", Count: 1, Seconds: m[0].Seconds}) {
		t.Errorf("metrics = %+v", m)
	}
	_ = config.EventHealthDegraded
}
