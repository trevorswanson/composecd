package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/selfapply"
	"github.com/trevorswanson/composecd/internal/store"
)

// fakeHelpers runs self-apply in-process against the test host, as the
// helper container would, and remembers what it started.
type fakeHelpers struct {
	mu    sync.Mutex
	specs []hosts.HelperSpec
	codes map[string]int
}

func (f *fakeHelpers) StartHelper(ctx context.Context, h *hosts.Host, spec hosts.HelperSpec) error {
	o, err := selfapply.Parse(spec.Args[1:])
	if err != nil {
		return err
	}
	code := 0
	if _, err := selfapply.Run(ctx, h.Configuration(nil, nil), o); err != nil {
		code = 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	f.codes[spec.Name] = code
	return nil
}

func (f *fakeHelpers) WaitHelper(_ context.Context, _ *hosts.Host, name string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.codes[name]
	if !ok {
		return 0, errors.New("no such container: " + name)
	}
	return code, nil
}

func (f *fakeHelpers) last() hosts.HelperSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[len(f.specs)-1]
}

// updatesSelf makes composecd's own container, from image sha256:self, part
// of compose project project on nas, and lets it start helpers.
func (e *env) updatesSelf(project string) *fakeHelpers {
	e.runAsSelf(project)
	e.fake().Objects["container/"+selfID]["Image"] = "sha256:self"
	f := &fakeHelpers{codes: map[string]int{}}
	e.c.Helpers = f
	return f
}

func (e *env) history(name string) string {
	e.t.Helper()
	h, err := e.backend.Store(e.c.Hosts.Get("nas")).History(name)
	if err != nil {
		e.t.Fatal(err)
	}
	var parts []string
	for _, r := range h {
		parts = append(parts, string(r.Status))
	}
	return strings.Join(parts, " ")
}

func TestSelfSyncThroughHelper(t *testing.T) {
	e := newEnv(t)
	helpers := e.updatesSelf("web")
	e.refresh()

	web := e.app("web")
	if !web.SelfUpdatable || web.Blocked != "" || web.Sync.Status != Synced {
		t.Fatalf("web: %+v", web)
	}
	spec := helpers.last()
	if spec.Image != "sha256:self" || spec.VolumesFrom != selfID || spec.Labels[labelSelfApply] == "" ||
		spec.Args[0] != "self-apply" || !contains(spec.Args, "--revision") {
		t.Errorf("helper spec = %+v", spec)
	}
	op := web.Operation
	if op == nil || op.Phase != store.PhaseSucceeded || op.Helper != spec.Name || op.ReleaseRevision != 1 ||
		!strings.Contains(op.Message, "composecd sync to") {
		t.Errorf("operation = %+v", op)
	}
	if !strings.Contains(e.fake().CallLog(), "rm "+spec.Name) {
		t.Errorf("the helper wasn't removed: %s", e.fake().CallLog())
	}

	// A broken version is rolled back by the helper.
	e.fake().FailUp = "nginx:bad"
	e.commit(map[string]string{"charts/web/values.yaml": "tag: bad\n"})
	e.refresh()
	web = e.app("web")
	if web.Operation.Phase != store.PhaseFailed || !strings.Contains(web.Operation.Message, "exited with code 1") {
		t.Errorf("operation = %+v", web.Operation)
	}
	if got := e.history("web"); got != "superseded failed deployed" {
		t.Errorf("history = %s", got)
	}
}

func TestSelfRollbackAndHealThroughHelper(t *testing.T) {
	e := newEnv(t)
	e.updatesSelf("web")
	e.refresh()
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '2'\n"})
	e.refresh()

	op, err := e.c.Rollback("web", 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	got, _ := e.c.Store.Operation(op.ID)
	if got.Phase != store.PhaseSucceeded || got.Helper == "" || got.ReleaseRevision != 3 {
		t.Errorf("rollback = %+v", got)
	}
	if st := e.app("web"); !st.Paused {
		t.Error("an automated app wasn't paused after its rollback")
	}

	o, err := selfapply.Parse([]string{"--release", "web", "--repair"})
	if err != nil || !o.Repair {
		t.Fatalf("parse --repair: %+v %v", o, err)
	}
}

func TestSelfUpdateWaitsForOtherOperations(t *testing.T) {
	e := newEnv(t)
	helpers := e.updatesSelf("web")
	drainTimeout, drainPoll = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { drainTimeout, drainPoll = 2*time.Minute, 200*time.Millisecond })

	e.c.mu.Lock()
	e.c.running["manual"] = 999
	e.c.mu.Unlock()
	e.refresh()
	if web := e.app("web"); web.Operation == nil || !strings.Contains(web.Operation.Message, "the operation on manual didn't finish") {
		t.Errorf("web operation = %+v", web.Operation)
	}
	if len(helpers.specs) != 0 {
		t.Error("a helper started while another operation ran")
	}

	// While composecd updates itself, nothing else starts.
	e.c.mu.Lock()
	delete(e.c.running, "manual")
	e.c.selfUpdating = "web"
	e.c.mu.Unlock()
	if _, err := e.c.Sync("manual", "admin"); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "updating itself") {
		t.Errorf("sync during a self-update: %v", err)
	}
}

func TestSelfRecoveryAfterRestart(t *testing.T) {
	e := newEnv(t)
	e.refresh() // deploys web at revision 1
	helpers := e.updatesSelf("web")
	h := e.c.Hosts.Get("nas")

	// The previous composecd prepared revision 2 and started a helper, which
	// finished after composecd was replaced.
	prepare := func() *release.Release {
		t.Helper()
		u := action.NewUpgrade(h.Configuration(nil, nil))
		u.Labels = map[string]string{LabelSourceRevision: "abc"}
		rel, err := u.Prepare(context.Background(), "web", e.repo+"/charts/web", map[string]interface{}{"tag": "2"})
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}
	rel := prepare()
	if _, err := action.NewApply(h.Configuration(nil, nil)).Run(context.Background(), "web", rel.Revision); err != nil {
		t.Fatal(err)
	}
	finished := &store.Operation{App: "web", Kind: store.KindSync, Trigger: store.TriggerAuto}
	must(t, e.c.Store.StartOperation(finished))
	must(t, e.c.Store.SetHelper(finished.ID, helperName(finished.ID)))
	helpers.codes[helperName(finished.ID)] = 0
	e.fake().Objects["container/"+helperName(finished.ID)] = map[string]interface{}{"State": map[string]interface{}{"Status": "exited"}}

	e.c.selfRecovered = false
	e.refresh()
	got, _ := e.c.Store.Operation(finished.ID)
	if got.Phase != store.PhaseSucceeded || got.ReleaseRevision != 2 || got.SourceRevision != "abc" {
		t.Errorf("recovered operation = %+v", got)
	}

	// Another one was cut off with its helper gone, leaving revision 3 pending.
	prepare()
	lost := &store.Operation{App: "web", Kind: store.KindSync, Trigger: store.TriggerAuto}
	must(t, e.c.Store.StartOperation(lost))
	must(t, e.c.Store.SetHelper(lost.ID, helperName(lost.ID)))
	e.c.selfRecovered = false
	e.refresh()
	got, _ = e.c.Store.Operation(lost.ID)
	if got.Phase != store.PhaseFailed || !strings.Contains(got.Message, "is gone") {
		t.Errorf("lost operation = %+v", got)
	}
	// (The recovered app synced back to Git in between.)
	if hist := e.history("web"); !strings.HasSuffix(hist, "deployed failed") {
		t.Errorf("history = %s", hist)
	}
}

func TestSelfApplyArgsRoundTrip(t *testing.T) {
	in := selfapply.Options{Release: "composecd", Revision: 4, RollbackTo: -1, Description: "composecd sync to abc",
		Labels: map[string]string{"b": "2", "a": "1"}, HistoryMax: 5, Storage: "local", LocalDir: "/data/hosts/nas/releases",
		Env: []string{"TZ=UTC", "A=b=c"}}
	in.Deploy.Wait, in.Deploy.Timeout, in.Deploy.Pull = true, 90*time.Second, "always"
	out, err := selfapply.Parse(in.Args())
	if err != nil {
		t.Fatal(err)
	}
	if out.Release != in.Release || out.Revision != 4 || out.RollbackTo != -1 || out.Description != in.Description ||
		out.Labels["a"] != "1" || out.HistoryMax != 5 || out.Storage != "local" || out.LocalDir != in.LocalDir ||
		strings.Join(out.Env, ",") != "TZ=UTC,A=b=c" || out.Deploy != in.Deploy {
		t.Errorf("round trip:\n in  %+v\n out %+v", in, out)
	}
	if _, err := selfapply.Parse([]string{"--release", "x"}); err == nil {
		t.Error("parsed options with nothing to do")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
