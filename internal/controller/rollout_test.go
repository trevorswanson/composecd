package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/compose/composetest"

	"github.com/trevorswanson/composecd/internal/store"
)

// dnsHosts adds two hosts labelled role: dns, each with its own fake engine.
const dnsHosts = hostsYAML + `---
apiVersion: composecd/v1alpha1
kind: Host
metadata: {name: dns1, labels: {role: dns}}
spec: {url: 'ssh://dns1'}
---
apiVersion: composecd/v1alpha1
kind: Host
metadata: {name: dns2, labels: {role: dns}}
spec: {url: 'ssh://dns2'}
`

// rollingSet is one app per dns host, rolled out with strategy.
func rollingSet(strategy string) string {
	return `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: unbound}
spec:
  generators:
    - hosts: {selector: {role: dns}}
  strategy:
` + strategy + `  template:
    metadata: {name: 'unbound-{{ .host.name }}', labels: {host: '{{ .host.name }}'}}
    spec:
      source: {path: charts/web}
      destination: {host: '{{ .host.name }}'}
      syncPolicy: {automated: {}, options: {wait: false}}
`
}

func newRolloutEnv(t *testing.T, strategy string) *env {
	t.Helper()
	debounce := eventDebounce
	eventDebounce = 10 * time.Millisecond
	t.Cleanup(func() { eventDebounce = debounce })
	e := newEnv(t)
	e.backend.fakes["dns1"] = composetest.New()
	e.backend.fakes["dns2"] = composetest.New()
	e.commit(map[string]string{"composecd/hosts.yaml": dnsHosts, "composecd/apps.yaml": "", "composecd/sets.yaml": rollingSet(strategy)})
	return e
}

// deployed is the commit unbound-<host> was last synced from.
func (e *env) deployed(host string) string {
	e.t.Helper()
	return e.app("unbound-" + host).Sync.DeployedRevision
}

// settled waits for the rollout's timers until cond holds.
func (e *env) settled(what string, cond func() bool) {
	e.t.Helper()
	eventually(e.t, what, func() bool {
		e.c.Wait()
		return cond()
	})
}

// quiet waits until no timer is pending and nothing it started still runs.
func (e *env) quiet() {
	e.t.Helper()
	eventually(e.t, "the rollout to settle", func() bool {
		e.c.Wait()
		e.c.mu.RLock()
		defer e.c.mu.RUnlock()
		return len(e.c.timers) == 0
	})
	e.c.Wait()
}

func lastOp(t *testing.T, e *env, name string) *store.Operation {
	t.Helper()
	ops, err := e.c.Operations(name, 1)
	if err != nil || len(ops) == 0 {
		t.Fatalf("operations of %s: %v %v", name, ops, err)
	}
	return ops[0]
}

func rolloutStates(e *env) string {
	r := e.c.ApplicationSets()[0].Rollout
	if r == nil {
		return "no rollout"
	}
	var out []string
	for _, step := range r.Steps {
		for _, a := range step.Applications {
			out = append(out, a.Name+"="+a.State)
		}
	}
	return strings.Join(out, " ")
}

func TestRollingSyncOneHostAtATime(t *testing.T) {
	e := newRolloutEnv(t, "    type: RollingSync\n    rollingSync: {maxUpdate: 1}\n")
	v1 := e.git("rev-parse", "HEAD")
	e.refresh()
	e.settled("the first deploy of both hosts", func() bool { return e.deployed("dns1") == v1 && e.deployed("dns2") == v1 })
	// Neither was deployed: dns1 went first, and dns2 only once it was Healthy.
	if a, b := lastOp(t, e, "unbound-dns1"), lastOp(t, e, "unbound-dns2"); b.StartedAt.Before(*a.FinishedAt) {
		t.Errorf("dns2 started at %s, before dns1 finished at %s", b.StartedAt, a.FinishedAt)
	}
	if got := rolloutStates(e); got != "unbound-dns1=Done unbound-dns2=Done" {
		t.Errorf("rollout = %s", got)
	}

	// A failed sync on the first host halts the rollout: the second is untouched.
	e.quiet()
	e.backend.fakes["dns1"].FailUp = "nginx:2"
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	e.quiet()
	if e.deployed("dns2") != v1 || lastOp(t, e, "unbound-dns1").Phase != store.PhaseFailed {
		t.Fatalf("dns2 was updated after dns1 failed: %s", e.deployed("dns2"))
	}
	dns2 := e.app("unbound-dns2")
	if !strings.HasPrefix(dns2.Blocked, rolloutPrefix) || !strings.Contains(dns2.Blocked, "unbound-dns1 (automatic sync failed") {
		t.Errorf("dns2 blocked = %q", dns2.Blocked)
	}
	if got := rolloutStates(e); got != "unbound-dns1=Failed unbound-dns2=Pending" {
		t.Errorf("rollout = %s", got)
	}

	// A fix in Git reaches the broken host first, then the rollout carries on.
	e.backend.fakes["dns1"].FailUp = ""
	v3 := e.commit(map[string]string{"charts/web/values.yaml": "tag: '3'\n"})
	e.refresh()
	e.settled("the fix on both hosts", func() bool { return e.deployed("dns2") == v3 })
	if a, b := lastOp(t, e, "unbound-dns1"), lastOp(t, e, "unbound-dns2"); a.Phase != store.PhaseSucceeded || b.StartedAt.Before(*a.FinishedAt) {
		t.Errorf("dns1 %+v, dns2 %+v", a, b)
	}

	// An app that is unhealthy after its sync halts the rollout too.
	e.quiet()
	f := e.backend.fakes["dns1"]
	f.Containers = map[string][]compose.Container{"unbound-dns1": {{Name: "unbound-dns1-web-1", Service: "web", State: "restarting"}}}
	v4 := e.commit(map[string]string{"charts/web/values.yaml": "tag: '4'\n"})
	e.refresh()
	e.quiet()
	if e.deployed("dns1") != v4 || e.deployed("dns2") != v3 {
		t.Fatalf("dns1 at %s, dns2 at %s", e.deployed("dns1"), e.deployed("dns2"))
	}
	if b := e.app("unbound-dns2").Blocked; !strings.Contains(b, "unbound-dns1 (Degraded)") {
		t.Errorf("dns2 blocked = %q", b)
	}
	// Once it recovers, the next host updates.
	f.Containers = nil
	e.refresh()
	e.settled("dns2 after dns1 recovered", func() bool { return e.deployed("dns2") == v4 })
}

// TestRollingSyncHealthyWaitsForBrokenSibling: updating the only healthy
// copy while the other is down would take the service down entirely.
func TestRollingSyncHealthyWaitsForBrokenSibling(t *testing.T) {
	strategy := "    type: RollingSync\n    rollingSync: {maxUpdate: 1}\n"
	e := newRolloutEnv(t, strategy)
	v1 := e.git("rev-parse", "HEAD")
	e.refresh()
	e.settled("the first deploy", func() bool { return e.deployed("dns2") == v1 })
	e.quiet()
	e.backend.fakes["dns2"].Containers = map[string][]compose.Container{"unbound-dns2": {{Name: "unbound-dns2-web-1", Service: "web",
		State: "running", Health: "unhealthy"}}}
	// Only dns1's values change, so dns2 stays in sync, and degraded.
	e.commit(map[string]string{"composecd/sets.yaml": strings.Replace(rollingSet(strategy),
		"source: {path: charts/web}", `source: {path: charts/web, values: {tag: '{{ if eq .host.name "dns1" }}2{{ else }}1{{ end }}'}}`, 1)})
	e.refresh()
	e.quiet()
	if e.deployed("dns1") != v1 {
		t.Fatal("dns1 was updated while dns2 was degraded")
	}
	if b := e.app("unbound-dns1").Blocked; !strings.Contains(b, "unbound-dns2 (Degraded)") {
		t.Errorf("dns1 blocked = %q", b)
	}
	// A manual sync still works: the rollout only holds automation back.
	if _, err := e.c.Sync("unbound-dns1", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if e.deployed("dns1") == v1 {
		t.Error("manual sync didn't deploy")
	}
}

func TestRollingSyncMinHealthyTime(t *testing.T) {
	e := newRolloutEnv(t, "    type: RollingSync\n    rollingSync: {maxUpdate: 1, minHealthyTime: 400ms}\n")
	v1 := e.git("rev-parse", "HEAD")
	e.refresh()
	e.settled("the first deploy", func() bool { return e.deployed("dns2") == v1 })
	v2 := e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	e.settled("dns2 waiting for dns1 to stay healthy", func() bool {
		return strings.Contains(e.app("unbound-dns2").Blocked, "unbound-dns1 (Healthy for")
	})
	e.settled("dns2 after dns1 stayed healthy", func() bool { return e.deployed("dns2") == v2 })
	a, b := lastOp(t, e, "unbound-dns1"), lastOp(t, e, "unbound-dns2")
	if gap := b.StartedAt.Sub(*a.FinishedAt); gap < 400*time.Millisecond {
		t.Errorf("dns2 started %s after dns1 finished, want at least minHealthyTime", gap)
	}
}

func TestRollingSyncSteps(t *testing.T) {
	strategy := `    type: RollingSync
    rollingSync:
      steps:
        - matchExpressions: [{key: host, operator: In, values: [dns1]}]
        - matchExpressions: [{key: host, operator: In, values: [dns2]}]
          maxUpdate: 0
`
	e := newRolloutEnv(t, strategy)
	v1 := e.git("rev-parse", "HEAD")
	e.refresh()
	e.settled("the first step", func() bool { return e.deployed("dns1") == v1 })
	e.quiet()
	if e.deployed("dns2") != "" {
		t.Fatal("a step with maxUpdate 0 was synced")
	}
	if b := e.app("unbound-dns2").Blocked; b != rolloutPrefix+"step 2 is held (maxUpdate 0)" {
		t.Errorf("dns2 blocked = %q", b)
	}
	// dns2, left out of every step, syncs only by hand.
	e.commit(map[string]string{"composecd/sets.yaml": rollingSet(strings.Replace(strategy, "values: [dns2]", "values: [other]", 1))})
	e.refresh()
	e.quiet()
	if b := e.app("unbound-dns2").Blocked; !strings.Contains(b, "no step selects this application") || e.deployed("dns2") != "" {
		t.Errorf("dns2 blocked = %q", b)
	}
	if r := e.c.ApplicationSets()[0].Rollout; r == nil || strings.Join(r.Unselected, ",") != "unbound-dns2" || r.Steps[1].MaxUpdate != 0 {
		t.Errorf("rollout = %+v", r)
	}
	// Later steps wait for every app of the earlier ones.
	e.backend.fakes["dns1"].FailUp = "nginx:2"
	e.commit(map[string]string{"composecd/sets.yaml": rollingSet(strings.Replace(strategy, "maxUpdate: 0", "maxUpdate: 50%", 1)),
		"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()
	e.quiet()
	if b := e.app("unbound-dns2").Blocked; !strings.Contains(b, "waiting for step 1: unbound-dns1 (automatic sync failed") || e.deployed("dns2") != "" {
		t.Errorf("dns2 blocked = %q", b)
	}
}
