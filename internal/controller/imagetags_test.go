package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/images"
)

const nginxRepo = "docker.io/library/nginx"

// strategyApp is an automated Application named name with imageUpdates set to
// policy (a flow mapping's body).
func strategyApp(name, policy string) string {
	return app(name, "nas", "  syncPolicy: {automated: {}, options: {wait: false}, imageUpdates: {enabled: true, "+policy+"}}\n")
}

// newStrategyEnv is newImageEnv with nginx at tag in Git, the registry listing
// tags (all as digestA), and web on policy.
func newStrategyEnv(t *testing.T, tag, policy string, tags ...string) (*env, *fakeRegistry) {
	t.Helper()
	e, reg := newImageEnv(t, false)
	reg.setTags(nginxRepo, digestA, tags...)
	e.commit(map[string]string{"charts/web/values.yaml": "tag: '" + tag + "'\n"})
	e.apps(strategyApp("web", policy))
	return e, reg
}

// has is true when r received an event of type typ whose message ends in msg.
func (r *recorder) has(typ, msg string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == typ && strings.HasSuffix(e.Message, msg) {
			return true
		}
	}
	return false
}

func TestImageSemverStrategy(t *testing.T) {
	e, reg := newStrategyEnv(t, "1.27.1", `strategy: semver, constraint: "~1.27"`,
		"1.27.1", "1.27.3", "1.27.4-rc1", "1.28.0", "2.0.0", "latest")
	n := &recorder{}
	e.c.Notify = n

	// The first sight lists the tags, so the first sync deploys the pick.
	e.refresh()
	running := e.fake().Running["web"]
	if !strings.Contains(running, "image: nginx:1.27.3@"+digestA) {
		t.Fatalf("first sync didn't deploy the picked tag:\n%s", running)
	}
	web := e.app("web")
	im := imageOf(web, "web")
	if im == nil || im.Strategy != images.StrategySemver || im.Constraint != "~1.27" || im.Tag != "1.27.3" || im.Candidate != "1.27.3" ||
		im.Ref != nginxRepo+":1.27.3" || im.Pin != PinComposecd || im.UpdateAvailable || im.TagsCheckedAt.IsZero() {
		t.Errorf("image = %+v", im)
	}
	if web.Sync.Status != Synced {
		t.Errorf("web: %+v", web.Sync)
	}

	// A refresh never lists tags.
	reg.setTags(nginxRepo, digestB, "1.27.1", "1.27.3", "1.27.5", "1.28.0")
	lists := reg.count("tags:" + nginxRepo)
	e.refresh()
	if reg.count("tags:"+nginxRepo) != lists || !strings.Contains(e.fake().Running["web"], "nginx:1.27.3@") {
		t.Errorf("refresh listed tags (%d) or moved the tag:\n%s", reg.count("tags:"+nginxRepo)-lists, e.fake().Running["web"])
	}

	// Pause the app to see the update wait, with its reason and notification.
	if err := e.c.Pause("web", "testing"); err != nil {
		t.Fatal(err)
	}
	e.checkImages("web")
	web = e.app("web")
	want := "image nginx (web): 1.27.3 → 1.27.5"
	if web.Sync.Status != OutOfSync || !contains(web.Sync.Reasons, want) {
		t.Errorf("reasons = %q, want %q", web.Sync.Reasons, want)
	}
	if im := imageOf(web, "web"); im == nil || !im.UpdateAvailable || im.Tag != "1.27.3" || im.Candidate != "1.27.5" || im.Latest != digestB {
		t.Errorf("image after check = %+v", im)
	}
	if len(web.Sync.Changes) != 1 || !strings.Contains(web.Sync.Changes[0].Diff, "+image: nginx:1.27.5@"+digestB) {
		t.Errorf("diff = %+v", web.Sync.Changes)
	}
	if !n.has(config.EventImageUpdate, "web: nginx (web): 1.27.5 available (semver ~1.27)") {
		t.Errorf("events = %+v", n.events)
	}

	if err := e.c.Resume("web"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	e.refresh()
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.5@"+digestB) || e.app("web").Sync.Status != Synced {
		t.Fatalf("update not deployed:\n%s", e.fake().Running["web"])
	}

	// Rolling back restores the old tag and pauses auto-sync, so the update
	// isn't applied again.
	if _, err := e.c.Rollback("web", 1, "trevor"); err != nil {
		t.Fatal(err)
	}
	e.c.ops.Wait()
	e.refresh()
	e.checkImages("web")
	web = e.app("web")
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@"+digestA) || !web.Paused {
		t.Errorf("rollback: paused %v\n%s", web.Paused, e.fake().Running["web"])
	}
	if !contains(web.Sync.Reasons, "image nginx (web): 1.27.3 → 1.27.5") {
		t.Errorf("reasons after rollback = %q", web.Sync.Reasons)
	}
}

func TestImageStrategyGitPinsAndWarnings(t *testing.T) {
	t.Run("a digest pinned in Git is left alone", func(t *testing.T) {
		e, reg := newStrategyEnv(t, "1.27.1@"+digestC, "strategy: semver", "1.27.1", "1.28.0")
		e.refresh()
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.1@"+digestC+"\n") {
			t.Errorf("Git's pin changed:\n%s", e.fake().Running["web"])
		}
		e.checkImages("web")
		im := imageOf(e.app("web"), "web")
		if im == nil || im.Pin != PinGit || im.Candidate != "" || !im.UpdateAvailable || im.Applicable || reg.count("tags:"+nginxRepo) != 0 {
			t.Errorf("image = %+v (tag lists %d)", im, reg.count("tags:"+nginxRepo))
		}
	})

	t.Run("semver on a tag that isn't semver pins its digest", func(t *testing.T) {
		e, reg := newStrategyEnv(t, "latest", "strategy: semver", "latest", "1.28.0")
		reg.set(nginxRepo+":latest", digestB)
		e.refresh()
		web := e.app("web")
		im := imageOf(web, "web")
		if !strings.Contains(e.fake().Running["web"], "image: nginx:latest@"+digestB) || im == nil || !strings.Contains(im.Note, "isn't a semantic version") ||
			web.Error != "" || web.Sync.Status != Synced {
			t.Errorf("image = %+v, app %+v\n%s", im, web.Sync, e.fake().Running["web"])
		}
	})

	t.Run("a truncated tag list is reported", func(t *testing.T) {
		e, reg := newStrategyEnv(t, "1.27.1", "strategy: semver", "1.27.1", "1.27.2")
		reg.mu.Lock()
		reg.truncated = true
		reg.mu.Unlock()
		e.refresh()
		if im := imageOf(e.app("web"), "web"); im == nil || im.Candidate != "1.27.2" || !strings.Contains(im.Note, "more tags") {
			t.Errorf("image = %+v", im)
		}
	})

	t.Run("a service label beats the Application", func(t *testing.T) {
		e, _ := newStrategyEnv(t, "1.27.1", "strategy: semver", "1.27.1", "1.27.2", "1.28.0")
		e.commit(map[string]string{"charts/web/templates/a.yaml": "services:\n  web:\n    image: nginx:{{ .Values.tag }}\n" +
			"    labels: {" + config.LabelImageUpdates + ".constraint: \"~1.27\"}\n" +
			"  side:\n    image: nginx:{{ .Values.tag }}\n    labels: {" + config.LabelImageUpdates + ".strategy: digest}\n"})
		e.refresh()
		running := e.fake().Running["web"]
		if !strings.Contains(running, "image: nginx:1.27.2@"+digestA) || !strings.Contains(running, "image: nginx:1.27.1@"+digestA) {
			t.Errorf("labels:\n%s", running)
		}
	})

	t.Run("a registry outage on first sight keeps Git's tag", func(t *testing.T) {
		e, reg := newStrategyEnv(t, "1.27.1", "strategy: semver", "1.27.1", "1.27.2")
		reg.setDown(true)
		e.refresh()
		calls := reg.total()
		e.refresh()
		if reg.total() != calls {
			t.Errorf("a failed first sight was retried on refresh")
		}
		if im := imageOf(e.app("web"), "web"); im == nil || im.Error == "" || im.Candidate != "" {
			t.Errorf("image during outage = %+v", im)
		}
		reg.setDown(false)
		e.checkImages("web")
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.2@"+digestA) {
			t.Errorf("not updated after the outage:\n%s", e.fake().Running["web"])
		}
	})
}

func TestImageStrategyOutageNeverDowngrades(t *testing.T) {
	deploy := func(t *testing.T) (*env, *fakeRegistry) {
		t.Helper()
		e, reg := newStrategyEnv(t, "1.27.1", `strategy: semver, constraint: "~1.27"`, "1.27.1", "1.27.3")
		e.refresh()
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@"+digestA) {
			t.Fatalf("not deployed:\n%s", e.fake().Running["web"])
		}
		reg.setDown(true)
		return e, reg
	}

	t.Run("a changed Git tag keeps the newer deployed tag", func(t *testing.T) {
		e, _ := deploy(t)
		e.commit(map[string]string{"charts/web/values.yaml": "tag: '1.27.2'\n"})
		e.refresh()
		web := e.app("web")
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@"+digestA) || e.ups("web") != 1 {
			t.Errorf("outage downgraded (ups %d):\n%s", e.ups("web"), e.fake().Running["web"])
		}
		im := imageOf(web, "web")
		if web.Sync.Status != Synced || im == nil || im.Tag != "1.27.3" || im.Digest != digestA || im.Error == "" ||
			!strings.Contains(im.Note, "registry unreachable; keeping the deployed 1.27.3") {
			t.Errorf("sync %+v, image %+v", web.Sync, im)
		}
	})

	t.Run("a narrowed constraint deploys Git's tag", func(t *testing.T) {
		e, _ := deploy(t)
		e.apps(strategyApp("web", `strategy: semver, constraint: "<1.27.3"`))
		e.refresh()
		running := e.fake().Running["web"]
		if strings.Contains(running, "1.27.3") || !strings.Contains(running, "image: nginx:1.27.1") {
			t.Errorf("want Git's tag:\n%s", running)
		}
	})
}

func TestImageNewestBuildStrategy(t *testing.T) {
	defer func(n int) { images.NewestBuildCandidates = n }(images.NewestBuildCandidates)
	images.NewestBuildCandidates = 2
	const repo = "docker.io/me/app"
	e, reg := newImageEnv(t, false)
	e.commit(map[string]string{"charts/web/templates/a.yaml": "services:\n  web:\n    image: me/app:{{ .Values.tag }}\n",
		"charts/web/values.yaml": "tag: main-1\n"})
	e.apps(strategyApp("web", "strategy: newest-build, allowTags: '^main-'"))
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// main-1 was built last, but only the two newest names and Git's tag are
	// read: main-9 is the newest of those.
	built := map[string]time.Time{"main-1": day, "main-2": day.Add(3 * time.Hour), "main-9": day.Add(2 * time.Hour), "main-10": day.Add(time.Hour)}
	digests := map[string]string{"main-1": digestA, "main-2": digestB, "main-9": digestC, "main-10": "sha256:" + strings.Repeat("d", 64)}
	reg.mu.Lock()
	reg.tags[repo] = []string{"dev", "main-1", "main-10", "main-2", "main-9"}
	for tag, d := range digests {
		reg.digests[repo+":"+tag] = d
		reg.created[d] = built[tag]
	}
	reg.created[digestA] = day.Add(4 * time.Hour)
	reg.mu.Unlock()

	e.refresh()
	if !strings.Contains(e.fake().Running["web"], "image: me/app:main-1@"+digestA) {
		t.Errorf("Git's tag, built last, should stay:\n%s", e.fake().Running["web"])
	}
	if reg.count("created:"+digestB) != 0 {
		t.Error("read a tag beyond the cap")
	}

	reg.mu.Lock()
	reg.created[digestA] = day
	reg.mu.Unlock()
	e.c.buildTimes.Clear()
	e.checkImages("web")
	if !strings.Contains(e.fake().Running["web"], "image: me/app:main-9@"+digestC) {
		t.Errorf("want main-9:\n%s", e.fake().Running["web"])
	}
	// Build times are cached by digest.
	before := reg.count("created:" + digestC)
	e.checkImages("web")
	if reg.count("created:"+digestC) != before {
		t.Error("build time read again")
	}
}
