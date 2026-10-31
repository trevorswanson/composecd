package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/images"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// fakeRegistry serves digests by registry/repository:tag, or fails when down.
type fakeRegistry struct {
	mu      sync.Mutex
	digests map[string]string
	down    bool
	calls   map[string]int
}

func (f *fakeRegistry) Digest(_ context.Context, r images.Ref) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.Name()]++
	if f.down {
		return "", errors.New("registry unreachable")
	}
	d, ok := f.digests[r.Name()]
	if !ok {
		return "", errors.New(r.Name() + ": not found")
	}
	return d, nil
}

func (f *fakeRegistry) set(ref, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[ref] = digest
}

func (f *fakeRegistry) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeRegistry) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

const nginx1 = "docker.io/library/nginx:1"

// newImageEnv is newEnv with a fake registry serving nginx:1 as digestA, and
// image updates on globally when global.
func newImageEnv(t *testing.T, global bool) (*env, *fakeRegistry) {
	t.Helper()
	e := newEnv(t)
	reg := &fakeRegistry{digests: map[string]string{nginx1: digestA}, calls: map[string]int{}}
	e.c.ImageResolver = reg
	e.c.Boot.ImageUpdates.Enabled = global
	return e, reg
}

func (e *env) checkImages(app string) {
	e.t.Helper()
	if err := e.c.CheckImages(app); err != nil {
		e.t.Fatal(err)
	}
	e.c.ops.Wait()
}

func imageOf(st AppStatus, svc string) *ImageStatus {
	for i := range st.Images {
		if st.Images[i].Service == svc {
			return &st.Images[i]
		}
	}
	return nil
}

func TestImageUpdatesPinMovingTags(t *testing.T) {
	e, reg := newImageEnv(t, true)
	e.refresh()

	web := e.app("web")
	if web.Sync.Status != Synced || e.ups("web") != 1 || web.Error != "" {
		t.Fatalf("web: %+v (ups %d)", web.Sync, e.ups("web"))
	}
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1@"+digestA) {
		t.Errorf("first sync not pinned:\n%s", e.fake().Running["web"])
	}
	if im := imageOf(web, "web"); im == nil || im.Pin != PinComposecd || im.Digest != digestA || im.Deployed != digestA ||
		im.Latest != digestA || im.UpdateAvailable || !im.Applicable || im.Ref != nginx1 {
		t.Errorf("web image = %+v", im)
	}
	if c := reg.total(); c < 1 || c > 2 {
		t.Errorf("registry calls = %d, want the first sight only", c)
	}

	// Refreshes don't ask the registry, and nothing changes.
	calls := reg.total()
	e.refresh()
	if reg.total() != calls || e.ups("web") != 1 {
		t.Errorf("refresh asked the registry (%d calls) or synced (%d ups)", reg.total()-calls, e.ups("web"))
	}

	// Deploy the manual app by hand, so its update waits.
	if _, err := e.c.Sync("manual", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	if st := e.app("manual"); st.Sync.Status != Synced {
		t.Fatalf("manual: %+v", st.Sync)
	}

	reg.set(nginx1, digestB)
	e.checkImages("")
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1@"+digestB) || e.ups("web") != 2 {
		t.Errorf("web not updated (ups %d):\n%s", e.ups("web"), e.fake().Running["web"])
	}
	if st := e.app("web"); st.Sync.Status != Synced || st.Release.Revision != 2 {
		t.Errorf("web after update: %+v release %+v", st.Sync, st.Release)
	}
	manual := e.app("manual")
	want := "image nginx:1 (web): " + images.Short(digestA) + " → " + images.Short(digestB)
	if manual.Sync.Status != OutOfSync || !contains(manual.Sync.Reasons, want) {
		t.Errorf("manual reasons = %q, want %q", manual.Sync.Reasons, want)
	}
	if im := imageOf(manual, "web"); im == nil || !im.UpdateAvailable || !im.Applicable || im.Deployed != digestA || im.Latest != digestB {
		t.Errorf("manual image = %+v", im)
	}
	if len(manual.Sync.Changes) != 1 || !strings.Contains(manual.Sync.Changes[0].Diff, "+image: nginx:1@"+digestB) {
		t.Errorf("manual diff = %+v", manual.Sync.Changes)
	}

	// Rollback restores the old digest.
	if _, err := e.c.Rollback("web", 1, "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1@"+digestA) {
		t.Errorf("rollback didn't restore the old digest:\n%s", e.fake().Running["web"])
	}
}

// overrideChart replaces the web chart's template with two services: web
// (nginx:<tag>) and side (busybox:1), side labelled with sideLabel if set.
func overrideChart(e *env, sideLabel string) {
	tpl := "services:\n  web:\n    image: nginx:{{ .Values.tag }}\n  side:\n    image: busybox:1\n"
	if sideLabel != "" {
		tpl += "    labels:\n      " + config.LabelImageUpdates + ": \"" + sideLabel + "\"\n"
	}
	e.commit(map[string]string{"charts/web/templates/a.yaml": tpl})
}

func appWithImageUpdates(name string, enabled bool) string {
	return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n" +
		"  source: {path: charts/web}\n  destination: {host: nas}\n" +
		"  syncPolicy: {automated: {}, options: {wait: false}, imageUpdates: {enabled: " + map[bool]string{true: "true", false: "false"}[enabled] + "}}\n"
}

func TestImageUpdatesOverrides(t *testing.T) {
	const busybox = "docker.io/library/busybox:1"
	pinned := func(e *env, app, image string) bool {
		return strings.Contains(e.fake().Running[app], "image: "+image+"@")
	}

	t.Run("app opts in while global is off", func(t *testing.T) {
		e, reg := newImageEnv(t, false)
		reg.set(busybox, digestC)
		e.commit(map[string]string{"composecd/apps.yaml": appYAML("web", true, "") + "---\n" + appWithImageUpdates("other", true)})
		overrideChart(e, "false")
		e.refresh()
		if pinned(e, "web", "nginx:1") || pinned(e, "web", "busybox:1") || imageOf(e.app("web"), "web") != nil {
			t.Errorf("web pinned with image updates off:\n%s", e.fake().Running["web"])
		}
		if !pinned(e, "other", "nginx:1") || pinned(e, "other", "busybox:1") {
			t.Errorf("other: want web pinned and side (labelled off) not:\n%s", e.fake().Running["other"])
		}
		if reg.calls[busybox] != 0 {
			t.Errorf("busybox checked although its label opts out")
		}
	})

	t.Run("app opts out while global is on", func(t *testing.T) {
		e, _ := newImageEnv(t, true)
		e.commit(map[string]string{"composecd/apps.yaml": appYAML("web", true, "") + "---\n" + appWithImageUpdates("other", false)})
		e.refresh()
		if !pinned(e, "web", "nginx:1") || pinned(e, "other", "nginx:1") {
			t.Errorf("web:\n%s\nother:\n%s", e.fake().Running["web"], e.fake().Running["other"])
		}
	})

	t.Run("service label opts in while the app is off", func(t *testing.T) {
		e, reg := newImageEnv(t, false)
		reg.set(busybox, digestC)
		overrideChart(e, "true")
		e.refresh()
		if pinned(e, "web", "nginx:1") || !strings.Contains(e.fake().Running["web"], "image: busybox:1@"+digestC) {
			t.Errorf("web:\n%s", e.fake().Running["web"])
		}
	})
}

func TestGitPinnedImagesAreNeverRewritten(t *testing.T) {
	e, reg := newImageEnv(t, true)
	reg.set(nginx1, digestB)
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '1@" + digestA + "'\n"})
	e.refresh()

	web := e.app("web")
	running := e.fake().Running["web"]
	if strings.Contains(running, digestB) || !strings.Contains(running, "image: nginx:1@"+digestA+"\n") {
		t.Errorf("Git's pin was changed:\n%s", running)
	}
	if web.Sync.Status != Synced || len(web.Sync.Reasons) != 0 {
		t.Errorf("a Git pin behind its tag must not be out of sync: %+v", web.Sync)
	}
	im := imageOf(web, "web")
	if im == nil || im.Pin != PinGit || !im.UpdateAvailable || im.Applicable || im.Latest != digestB || im.Digest != digestA ||
		im.Deployed != digestA || im.Note != gitPinNote {
		t.Errorf("image = %+v", im)
	}

	// Neither a check nor a manual sync applies it.
	e.checkImages("web")
	if _, err := e.c.Sync("web", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	if strings.Contains(e.fake().Running["web"], digestB) {
		t.Errorf("sync applied an update to a Git pin:\n%s", e.fake().Running["web"])
	}
}

func TestImageRegistryOutageKeepsPins(t *testing.T) {
	e, reg := newImageEnv(t, true)
	e.refresh()
	if !strings.Contains(e.fake().Running["web"], "@"+digestA) {
		t.Fatalf("not pinned:\n%s", e.fake().Running["web"])
	}

	// A failed check keeps the last digest.
	reg.setDown(true)
	e.checkImages("")
	web := e.app("web")
	if web.Sync.Status != Synced || e.ups("web") != 1 {
		t.Errorf("outage changed web: %+v (ups %d)", web.Sync, e.ups("web"))
	}
	if im := imageOf(web, "web"); im == nil || im.Digest != digestA || im.Error == "" {
		t.Errorf("image during outage = %+v", im)
	}

	// With the cache gone too, the deployed release's digest is kept, and the
	// failure isn't retried on every refresh.
	if err := e.c.Store.DeleteImageDigests(); err != nil {
		t.Fatal(err)
	}
	e.refresh()
	web = e.app("web")
	if web.Sync.Status != Synced || e.ups("web") != 1 || len(web.Sync.Changes) != 0 {
		t.Errorf("lost cache showed a change: %+v (ups %d)", web.Sync, e.ups("web"))
	}
	if im := imageOf(web, "web"); im == nil || im.Pin != PinComposecd || im.Digest != digestA {
		t.Errorf("image without a cache = %+v", im)
	}
	calls := reg.total()
	e.refresh()
	if reg.total() != calls {
		t.Errorf("a failed first check was retried on refresh")
	}

	// Back up: the next check resolves it again.
	reg.setDown(false)
	reg.set(nginx1, digestB)
	e.checkImages("web")
	if !strings.Contains(e.fake().Running["web"], "@"+digestB) {
		t.Errorf("not updated after the outage:\n%s", e.fake().Running["web"])
	}
}

func TestImageUpdateNotifies(t *testing.T) {
	e, reg := newImageEnv(t, true)
	n := &recorder{}
	e.c.Notify = n
	e.refresh()
	if _, err := e.c.Sync("manual", "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	reg.set(nginx1, digestB)
	e.checkImages("")
	// Each app reports the update once, however often it is reconciled.
	e.refresh()
	e.refresh()
	if got := n.types(); strings.Count(got, config.EventImageUpdate+":manual") != 1 || strings.Count(got, config.EventImageUpdate+":web") > 1 {
		t.Errorf("events = %s", n.types())
	}
}

func TestSelfUpdateAppliesPinnedImages(t *testing.T) {
	e, _ := newImageEnv(t, true)
	e.updatesSelf("web")
	e.refresh()
	web := e.app("web")
	if web.Sync.Status != Synced || web.Operation == nil || web.Operation.Helper == "" {
		t.Fatalf("web: %+v op %+v", web.Sync, web.Operation)
	}
	h, err := e.backend.Store(e.c.Hosts.Get("nas")).History("web")
	if err != nil || len(h) != 1 || !strings.Contains(h[0].Manifest, "image: nginx:1@"+digestA) {
		t.Errorf("the helper's revision isn't pinned: %v %+v", err, h)
	}
}
