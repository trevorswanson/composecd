package controller

import (
	"errors"
	"strings"
	"testing"
)

const selfID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestContainerIDFrom(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"docker", "1 0 0:1 / / rw - overlay overlay rw\n" +
			"2 1 254:1 /var/lib/docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n", selfID},
		{"podman", "3 1 0:2 /overlay-containers/" + selfID + "/userdata/hostname /etc/hostname rw - tmpfs tmpfs rw\n", selfID},
		{"containers on their own filesystem", "1 0 0:1 / / rw - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/" + strings.Repeat("e", 64) + "/diff\n" +
			"8583 8423 0:52 /" + selfID + "/hostname /etc/hostname rw,relatime - tmpfs tmpfs rw,inode64\n", selfID},
		{"no container","1 0 0:1 / / rw - ext4 /dev/sda1 rw\n", ""},
		{"short id", "2 1 254:1 /var/lib/docker/containers/abc123/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n", ""},
	} {
		if got := containerIDFrom(strings.NewReader(tc.in)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// runAsSelf makes composecd's own container part of compose project project
// on host nas.
func (e *env) runAsSelf(project string) {
	e.c.SelfContainer = selfID
	e.fake().Objects = map[string]map[string]interface{}{"container/" + selfID: {
		"Config": map[string]interface{}{"Labels": map[string]interface{}{labelComposeProject: project}},
	}}
}

func TestSelfIsDetectedAndNotSynced(t *testing.T) {
	e := newEnv(t)
	e.runAsSelf("web")
	e.refresh()

	web, manual := e.app("web"), e.app("manual")
	if !web.Self || manual.Self {
		t.Fatalf("self: web %v, manual %v", web.Self, manual.Self)
	}
	if web.Blocked != selfBlocked || e.ups("web") != 0 {
		t.Errorf("web was synced (blocked %q, ups %d)", web.Blocked, e.ups("web"))
	}
	if _, err := e.c.Sync("web", "admin"); !errors.Is(err, ErrSelf) {
		t.Errorf("manual sync: %v", err)
	}
	if _, err := e.c.Rollback("web", 0, "admin"); !errors.Is(err, ErrSelf) {
		t.Errorf("rollback: %v", err)
	}
	if _, err := e.c.Sync("manual", "admin"); err != nil {
		t.Errorf("syncing another app: %v", err)
	}
	if h := e.c.HostStatuses()[0]; h.Self != "web" {
		t.Errorf("host self = %q", h.Self)
	}

	// Our own container stopping during an update must not start a reconcile.
	e.c.onEvent("nas", "web")
	e.c.mu.RLock()
	_, scheduled := e.c.timers["web"]
	e.c.mu.RUnlock()
	if scheduled {
		t.Error("an event from composecd's own container scheduled a reconcile")
	}
}

func TestSelfIsNotPrunedOrDeleted(t *testing.T) {
	e := newEnv(t)
	e.apps(
		app("web", "nas", "  syncPolicy: {automated: {prune: true}, options: {wait: false}}\n"),
		app("other", "nas", "  syncPolicy: {automated: {}, options: {wait: false}}\n"),
	)
	e.refresh()
	if _, ok := e.fake().Running["web"]; !ok {
		t.Fatalf("web not deployed: %v", e.fake().Running)
	}

	// web turns out to be composecd itself, and leaves Git.
	e.runAsSelf("web")
	e.apps(app("other", "nas", "  syncPolicy: {automated: {}, options: {wait: false}}\n"))
	e.refresh()
	if _, ok := e.fake().Running["web"]; !ok {
		t.Fatal("composecd's own release was pruned")
	}
	if h := e.c.HostStatuses()[0]; strings.Join(h.Orphans, ",") != "web" {
		t.Errorf("orphans = %v", h.Orphans)
	}
	if _, err := e.c.DeleteRelease("nas", "web", "admin"); !errors.Is(err, ErrSelf) {
		t.Errorf("delete: %v", err)
	}
	u, err := e.c.Unmanaged(t.Context(), "nas")
	if err != nil || len(u.Releases) != 1 || !u.Releases[0].Self {
		t.Errorf("unmanaged releases = %+v, %v", u, err)
	}
}

func TestSelfOverride(t *testing.T) {
	e := newEnv(t)
	// composecd can't find its own container: spec.self marks the app.
	e.apps(
		app("web", "nas", "  self: true\n  syncPolicy: {automated: {}, options: {wait: false}}\n"),
		app("other", "nas", ""),
	)
	e.refresh()
	if web := e.app("web"); !web.Self || web.Error != "" || e.ups("web") != 0 {
		t.Errorf("web: self %v, error %q, ups %d", web.Self, web.Error, e.ups("web"))
	}

	// The override stays after the app leaves Git, so prune can't remove it.
	e.apps(app("other", "nas", ""))
	e.refresh()
	if !e.c.isSelf("nas", "web") {
		t.Error("spec.self was forgotten")
	}

	// It contradicts what composecd found.
	e.runAsSelf("other")
	e.apps(app("web", "nas", "  self: true\n"), app("other", "nas", ""))
	e.refresh()
	if web := e.app("web"); !strings.Contains(web.Error, `composecd runs as compose project "other"`) {
		t.Errorf("web error = %q", web.Error)
	}
}
