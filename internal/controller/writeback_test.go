package controller

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/chart"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/images"
	"github.com/trevorswanson/composecd/internal/source"
	"github.com/trevorswanson/composecd/internal/store"
)

// wbApp is an automated Application named name on chart path with semver
// updates within ~1.27 written to Git, and extra source fields (", key: v").
func wbApp(name, path, extra string) string {
	return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n" +
		"  source: {path: " + path + extra + "}\n  destination: {host: nas}\n" +
		"  syncPolicy: {automated: {}, options: {wait: false}, imageUpdates: {enabled: true, strategy: semver, constraint: \"~1.27\", writeBack: git}}\n---\n"
}

// newWriteBackEnv is newImageEnv whose repository takes composecd's pushes,
// with the registry listing nginx's tags (all as digestA) and files committed.
func newWriteBackEnv(t *testing.T, files map[string]string, tags ...string) (*env, *fakeRegistry) {
	t.Helper()
	e, reg := newImageEnv(t, false)
	e.git("config", "receive.denyCurrentBranch", "updateInstead")
	reg.setTags(nginxRepo, digestA, tags...)
	e.commit(files)
	return e, reg
}

// show is a file at HEAD of e's repository.
func (e *env) show(file string) string {
	e.t.Helper()
	return e.git("show", "HEAD:"+file) + "\n"
}

func TestWriteBackCommitsTheTag(t *testing.T) {
	values := "# web\ntag: '1.27.1'   # the release we run\nreplicas: 1\n"
	e, reg := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": values, "composecd/apps.yaml": wbApp("web", "charts/web", "")},
		"1.27.1", "1.27.3", "1.28.0")
	before := e.git("rev-parse", "HEAD")
	e.refresh()

	// The pick was committed in place, and deployed from Git.
	if got, want := e.show("charts/web/values.yaml"), strings.Replace(values, "1.27.1", "1.27.3", 1); got != want {
		t.Errorf("values.yaml:\n%s\nwant:\n%s", got, want)
	}
	msg := e.git("log", "-1", "--format=%an <%ae>%n%B")
	for _, want := range []string{"composecd <composecd@localhost>", "composecd: update images", "web/web: nginx 1.27.1 → 1.27.3 (charts/web/values.yaml tag)",
		TrailerImageUpdate} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message lacks %q:\n%s", want, msg)
		}
	}
	if parent := e.git("rev-parse", "HEAD~1"); parent != before {
		t.Errorf("parent = %s, want %s", parent, before)
	}
	head := e.git("rev-parse", "HEAD")
	if !e.c.ImageUpdateCommit(head) {
		t.Error("the commit isn't known as composecd's own")
	}
	web := e.app("web")
	im := imageOf(web, "web")
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@"+digestA) || web.Sync.Status != Synced || web.Sync.Revision != head ||
		im == nil || im.Candidate != "" || im.Diverged || web.Diverged {
		t.Fatalf("web %+v, image %+v\n%s", web.Sync, im, e.fake().Running["web"])
	}

	// Its arrival, through a webhook or a poll, lists no tags and commits
	// nothing more.
	lists := reg.count("tags:" + nginxRepo)
	e.refresh()
	if reg.count("tags:"+nginxRepo) != lists || e.git("rev-parse", "HEAD") != head {
		t.Errorf("a refresh after the commit listed tags (%d) or committed again", reg.count("tags:"+nginxRepo)-lists)
	}

	// A newer tag on the next check is written the same way.
	reg.setTags(nginxRepo, digestB, "1.27.1", "1.27.3", "1.27.4")
	e.checkImages("web")
	e.refresh()
	if got := e.show("charts/web/values.yaml"); !strings.Contains(got, "tag: '1.27.4'   # the release we run") {
		t.Errorf("values.yaml after a check:\n%s", got)
	}
	if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.4@"+digestB) {
		t.Errorf("not deployed:\n%s", e.fake().Running["web"])
	}
}

func TestWriteBackHolds(t *testing.T) {
	literal := "services:\n  web:\n    image: nginx:1.27.1\n    labels: {revision: \"{{ .Release.Revision }}\"}\n"
	setup := func(t *testing.T) (*env, *fakeRegistry, *recorder) {
		t.Helper()
		e, reg := newWriteBackEnv(t, map[string]string{"charts/web/templates/a.yaml": literal, "composecd/apps.yaml": wbApp("web", "charts/web", "")},
			"1.27.1", "1.27.3")
		n := &recorder{}
		e.c.Notify = n
		e.refresh()
		e.refresh()
		return e, reg, n
	}
	held := func(t *testing.T, e *env, tag string) *ImageStatus {
		t.Helper()
		web := e.app("web")
		im := imageOf(web, "web")
		if im == nil || im.Held == "" || im.Candidate != tag || !im.UpdateAvailable || im.Applicable || !strings.HasPrefix(im.Note, "Held: ") {
			t.Fatalf("image = %+v", im)
		}
		if web.Sync.Status != Synced || !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.1@"+digestA) {
			t.Fatalf("a held update deployed: %+v\n%s", web.Sync, e.fake().Running["web"])
		}
		return im
	}
	blocked := func(n *recorder) int {
		n.mu.Lock()
		defer n.mu.Unlock()
		c := 0
		for _, ev := range n.events {
			if ev.Type == config.EventWriteBackBlocked {
				c++
			}
		}
		return c
	}

	t.Run("nothing deploys until an admin decides", func(t *testing.T) {
		e, _, n := setup(t)
		head := e.git("rev-parse", "HEAD")
		im := held(t, e, "1.27.3")
		if !strings.Contains(im.Held, "it may be written in a template") {
			t.Errorf("held = %q", im.Held)
		}
		e.checkImages("web")
		e.refresh()
		held(t, e, "1.27.3")
		if got := blocked(n); got != 1 {
			t.Errorf("%d write-back-blocked notifications, want 1: %+v", got, n.events)
		}
		if e.git("rev-parse", "HEAD") != head {
			t.Error("a held update was committed")
		}
		if _, err := e.c.DecideImage("web", "web", "sideways", "trevor"); err == nil {
			t.Error("an unknown choice was accepted")
		}
		if _, err := e.c.DecideImage("web", "nope", store.DecisionMemory, "trevor"); err == nil {
			t.Error("a decision about an unknown service was accepted")
		}
	})

	t.Run("apply in memory deploys and shows the divergence", func(t *testing.T) {
		e, _, _ := setup(t)
		if _, err := e.c.DecideImage("web", "web", store.DecisionMemory, "trevor"); err != nil {
			t.Fatal(err)
		}
		e.c.ops.Wait()
		e.refresh()
		web := e.app("web")
		im := imageOf(web, "web")
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@"+digestA) || !web.Diverged || im == nil || !im.Diverged ||
			im.Decision != store.DecisionMemory || im.DecidedBy != "trevor" || im.Held != "" {
			t.Fatalf("web diverged %v, image %+v\n%s", web.Diverged, im, e.fake().Running["web"])
		}
		ops, err := e.c.Operations("web", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slicesContain(ops, func(op *store.Operation) bool {
			return op.Kind == store.KindImageDecision && op.User == "trevor" && strings.Contains(op.Message, "diverges from Git, which says 1.27.1")
		}) {
			t.Errorf("no image-decision operation: %+v", ops)
		}
		if _, err := e.c.DecideImage("web", "web", store.DecisionDismiss, "trevor"); err == nil {
			t.Error("decided about an update that isn't held")
		}

		// A change to the chart lapses the decision: here, it moves the tag
		// into the values, which write-back then edits.
		e.commit(map[string]string{"charts/web/templates/a.yaml": strings.Replace(literal, "1.27.1", "{{ .Values.tag }}", 1),
			"charts/web/values.yaml": "tag: 1.27.1\n"})
		e.refresh()
		web = e.app("web")
		if im := imageOf(web, "web"); web.Diverged || im == nil || im.Decision != "" || im.Candidate != "" {
			t.Errorf("after the chart changed: diverged %v, image %+v", web.Diverged, im)
		}
		if got := e.show("charts/web/values.yaml"); got != "tag: 1.27.3\n" {
			t.Errorf("values.yaml = %q", got)
		}
		if decisions, _ := e.c.Store.ImageDecisions("web"); len(decisions) != 0 {
			t.Errorf("decisions = %+v", decisions)
		}
	})

	t.Run("dismiss ignores the candidate until a newer one", func(t *testing.T) {
		e, reg, n := setup(t)
		if _, err := e.c.DecideImage("web", "web", store.DecisionDismiss, "trevor"); err != nil {
			t.Fatal(err)
		}
		e.c.ops.Wait()
		e.refresh()
		e.checkImages("web")
		im := imageOf(e.app("web"), "web")
		if im == nil || im.Held != "" || im.UpdateAvailable || im.Decision != store.DecisionDismiss || !strings.Contains(im.Note, "dismissed by trevor") ||
			!strings.Contains(e.fake().Running["web"], "image: nginx:1.27.1@"+digestA) {
			t.Fatalf("dismissed image = %+v\n%s", im, e.fake().Running["web"])
		}
		reg.setTags(nginxRepo, digestA, "1.27.1", "1.27.3", "1.27.5")
		e.checkImages("web")
		held(t, e, "1.27.5")
		if got := blocked(n); got != 2 {
			t.Errorf("%d write-back-blocked notifications, want 2", got)
		}
	})
}

func slicesContain[T any](s []T, f func(T) bool) bool {
	for _, v := range s {
		if f(v) {
			return true
		}
	}
	return false
}

func TestWriteBackFileChoice(t *testing.T) {
	t.Run("the last layer setting the key is edited", func(t *testing.T) {
		e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: '1.0'\n", "charts/web/prod.yaml": "tag: 1.27.1 # prod\n",
			"composecd/apps.yaml": wbApp("web", "charts/web", ", valueFiles: [prod.yaml]")}, "1.27.1", "1.27.3")
		e.refresh()
		if got := e.show("charts/web/prod.yaml"); got != "tag: 1.27.3 # prod\n" {
			t.Errorf("prod.yaml = %q", got)
		}
		if got := e.show("charts/web/values.yaml"); got != "tag: '1.0'\n" {
			t.Errorf("values.yaml = %q", got)
		}
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@") {
			t.Errorf("not deployed:\n%s", e.fake().Running["web"])
		}
	})

	t.Run("a values file other apps read gets the key in the app's own file", func(t *testing.T) {
		e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: 1.27.1\n", "charts/web/web.yaml": "# web's own\nreplicas: 2\n",
			"composecd/apps.yaml": wbApp("web", "charts/web", ", valueFiles: [web.yaml]") + app("web2", "nas", "  syncPolicy: {automated: {}, options: {wait: false}}\n")},
			"1.27.1", "1.27.3")
		e.refresh()
		if got := e.show("charts/web/values.yaml"); got != "tag: 1.27.1\n" {
			t.Errorf("the shared values.yaml changed: %q", got)
		}
		if got := e.show("charts/web/web.yaml"); got != "# web's own\nreplicas: 2\ntag: \"1.27.3\"\n" {
			t.Errorf("web.yaml = %q", got)
		}
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@") || !strings.Contains(e.fake().Running["web2"], "image: nginx:1.27.1\n") {
			t.Errorf("web:\n%s\nweb2:\n%s", e.fake().Running["web"], e.fake().Running["web2"])
		}
	})

	t.Run("a shared file without an app file of its own holds", func(t *testing.T) {
		e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: 1.27.1\n",
			"composecd/apps.yaml": wbApp("web", "charts/web", "") + app("web2", "nas", "")}, "1.27.1", "1.27.3")
		e.refresh()
		if im := imageOf(e.app("web"), "web"); im == nil || !strings.Contains(im.Held, "which web2 also read") || im.File != "charts/web/values.yaml" || im.Key != "tag" {
			t.Errorf("image = %+v", im)
		}
	})

	t.Run("inline values in the Application", func(t *testing.T) {
		e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: '1.0'\n",
			"composecd/apps.yaml": wbApp("web", "charts/web", ", values: {tag: '1.27.1'}")}, "1.27.1", "1.27.3")
		e.refresh()
		e.refresh()
		if got := e.show("composecd/apps.yaml"); !strings.Contains(got, "source: {path: charts/web, values: {tag: '1.27.3'}}") {
			t.Errorf("apps.yaml:\n%s", got)
		}
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1.27.3@") {
			t.Errorf("not deployed:\n%s", e.fake().Running["web"])
		}
	})

	t.Run("a shared key is edited when every service it sets moves with it", func(t *testing.T) {
		tpl := "services:\n  web:\n    image: nginx:{{ .Values.global.tag }}\n  worker:\n    image: nginx:{{ .Values.global.tag }}\n"
		e, _ := newWriteBackEnv(t, map[string]string{"charts/web/templates/a.yaml": tpl, "charts/web/values.yaml": "global:\n  tag: 1.27.1\n",
			"composecd/apps.yaml": wbApp("web", "charts/web", "")}, "1.27.1", "1.27.3")
		e.refresh()
		if got := e.show("charts/web/values.yaml"); got != "global:\n  tag: 1.27.3\n" {
			t.Errorf("values.yaml = %q", got)
		}
		if msg := e.git("log", "-1", "--format=%B"); !strings.Contains(msg, "web/web: ") || !strings.Contains(msg, "web/worker: ") {
			t.Errorf("message:\n%s", msg)
		}

		// Not when one of them stays.
		e, _ = newWriteBackEnv(t, map[string]string{"charts/web/templates/a.yaml": tpl + "    labels: {" + config.LabelImageUpdates + ".strategy: digest}\n",
			"charts/web/values.yaml": "global:\n  tag: 1.27.1\n", "composecd/apps.yaml": wbApp("web", "charts/web", "")}, "1.27.1", "1.27.3")
		e.refresh()
		if im := imageOf(e.app("web"), "web"); im == nil || !strings.Contains(im.Held, "global.tag also sets the image of worker") {
			t.Errorf("image = %+v", im)
		}
	})

	t.Run("the targetRevision must be a branch", func(t *testing.T) {
		e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: 1.27.1\n"}, "1.27.1", "1.27.3")
		e.git("tag", "v1")
		e.apps(wbApp("web", "charts/web", ", targetRevision: v1"))
		e.refresh()
		if im := imageOf(e.app("web"), "web"); im == nil || !strings.Contains(im.Held, "imageUpdates.writeBack.branch") {
			t.Errorf("image = %+v", im)
		}
	})
}

func TestWriteBackBranchForReview(t *testing.T) {
	e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": "tag: 1.27.1\n", "composecd/apps.yaml": wbApp("web", "charts/web", "")},
		"1.27.1", "1.27.3")
	e.c.Boot.ImageUpdates.WriteBack.Branch = "image-updates"
	main := e.git("rev-parse", "main")
	e.refresh()
	if got := e.git("show", "image-updates:charts/web/values.yaml"); got != "tag: 1.27.3" {
		t.Errorf("image-updates has %q", got)
	}
	if e.git("rev-parse", "main") != main || e.git("rev-parse", "image-updates~1") != main {
		t.Error("main moved, or the branch didn't start at it")
	}
	commit := e.git("rev-parse", "image-updates")
	web := e.app("web")
	im := imageOf(web, "web")
	if im == nil || im.Commit != commit || im.Held != "" || web.Sync.Status != Synced || !strings.Contains(e.fake().Running["web"], "nginx:1.27.1@") {
		t.Fatalf("image = %+v, sync %+v", im, web.Sync)
	}
	// Checking again commits nothing more; merging deploys it.
	e.checkImages("web")
	if e.git("rev-parse", "image-updates") != commit {
		t.Error("committed again")
	}
	e.git("merge", "--quiet", "--ff-only", "image-updates")
	e.refresh()
	if !strings.Contains(e.fake().Running["web"], "nginx:1.27.3@") {
		t.Errorf("not deployed after the merge:\n%s", e.fake().Running["web"])
	}
}

func TestWriteBackRetriesARejectedPush(t *testing.T) {
	values := "tag: 1.27.1\n"
	e, _ := newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": values, "composecd/apps.yaml": wbApp("web", "charts/web", "")},
		"1.27.1", "1.27.3")
	pushes := 0
	e.c.beforePush = func(string) {
		// Someone else pushes a change to the same file first, twice.
		if pushes++; pushes <= 2 {
			e.commit(map[string]string{"charts/web/values.yaml": strings.Repeat("# theirs\n", pushes) + values})
		}
	}
	e.refresh()
	if got, want := e.show("charts/web/values.yaml"), "# theirs\n# theirs\ntag: 1.27.3\n"; got != want {
		t.Errorf("values.yaml = %q, want %q", got, want)
	}
	if pushes != 3 || !strings.Contains(e.git("log", "-1", "--format=%B"), TrailerImageUpdate) {
		t.Errorf("%d pushes", pushes)
	}

	// A push that keeps failing holds the update, and so does one the
	// remote refuses.
	e, _ = newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": values, "composecd/apps.yaml": wbApp("web", "charts/web", "")},
		"1.27.1", "1.27.3")
	n := 0
	e.c.beforePush = func(string) {
		n++
		e.commit(map[string]string{"README": strings.Repeat("x", n)})
	}
	e.refresh()
	if im := imageOf(e.app("web"), "web"); im == nil || !strings.Contains(im.Held, "the branch moved on") || n != writeBackRetries+1 {
		t.Errorf("image = %+v after %d pushes", im, n)
	}

	e, _ = newWriteBackEnv(t, map[string]string{"charts/web/values.yaml": values, "composecd/apps.yaml": wbApp("web", "charts/web", "")},
		"1.27.1", "1.27.3")
	e.git("config", "receive.denyCurrentBranch", "refuse")
	e.refresh()
	if im := imageOf(e.app("web"), "web"); im == nil || !strings.Contains(im.Held, "writing to Git") ||
		!strings.Contains(e.fake().Running["web"], "nginx:1.27.1@") {
		t.Errorf("image = %+v", im)
	}
}

func TestWriteBackRefusesWhatItCantEdit(t *testing.T) {
	const repo = "https://git.example.lan/me/gitops.git"
	dir := t.TempDir()
	for name, content := range map[string]string{
		"charts/web/values.yaml":       "tag: 1.0\n",
		"charts/web/secrets.sops.yaml": "tag: ENC[AES256_GCM,data:eA==,iv:aQ==,tag:dA==,type:str]\nsops:\n  version: 3.9.0\n",
		"values/web.yaml":              "replicas: 1\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitApp := func(extra func(*config.Application)) *config.Application {
		a := &config.Application{Metadata: config.Metadata{Name: "web"},
			Spec: config.AppSpec{Source: config.AppSource{RepoURL: repo, Path: "charts/web"}}}
		if extra != nil {
			extra(a)
		}
		return a
	}
	for name, c := range map[string]struct {
		app     *config.Application
		secrets [][]string
		values  map[string]interface{}
		want    string
	}{
		"SOPS": {
			app:  gitApp(func(a *config.Application) { a.Spec.Source.ValueFiles = []string{"secrets.sops.yaml"} }),
			want: "the tag is in charts/web/secrets.sops.yaml, which SOPS encrypts",
		},
		"Infisical": {
			app: gitApp(func(a *config.Application) {
				a.Spec.Source.Infisical = &config.InfisicalSource{ProjectID: "p", Environment: "prod"}
			}),
			secrets: [][]string{{"tag"}},
			want:    "the tag comes from Infisical",
		},
		"an ApplicationSet's inline values": {
			app: gitApp(func(a *config.Application) {
				a.Metadata.Labels = map[string]string{LabelApplicationSet: "stacks"}
				a.Spec.Source.Values = map[string]interface{}{"tag": "1.27.1"}
			}),
			want: "the inline values of ApplicationSet stacks's template",
		},
		"an OCI chart's packaged values": {
			app: &config.Application{Metadata: config.Metadata{Name: "web"}, Spec: config.AppSpec{Source: config.AppSource{
				Chart: "oci://ghcr.io/me/charts/web", ValuesFrom: &config.GitSource{RepoURL: repo}}}},
			values: map[string]interface{}{"tag": "1.27.1"},
			want:   "the OCI chart's packaged values, and web has no Git values file of its own",
		},
		"an OCI chart's packaged values, overridden in the app's file": {
			app: &config.Application{Metadata: config.Metadata{Name: "web"}, Spec: config.AppSpec{Source: config.AppSource{
				Chart: "oci://ghcr.io/me/charts/web", ValuesFrom: &config.GitSource{RepoURL: repo}, ValueFiles: []string{"values/web.yaml"}}}},
			values: map[string]interface{}{"tag": "1.27.1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctl := &Controller{Options: Options{Boot: &config.Bootstrap{Root: config.Root{RepoURL: repo, TargetRevision: "main"}}}}
			j := &wbJob{app: c.app, key: "tag", tag: "1.27.3"}
			tr := &tracer{chart: &chart.Chart{Values: c.values, Metadata: &chart.Metadata{}}}
			src := &repoTip{url: repo, branch: "main", sha: strings.Repeat("a", 40), dir: dir}
			ctl.locate(&config.Objects{Applications: []*config.Application{c.app}}, j, &source.Prepared{SecretPaths: c.secrets}, tr, src, "1.27.3", nil)
			switch {
			case c.want == "" && (j.held != "" || j.edit == nil || j.edit.path != "values/web.yaml"):
				t.Errorf("held %q, edit %+v", j.held, j.edit)
			case c.want != "" && !strings.Contains(j.held, c.want):
				t.Errorf("held = %q, want %q", j.held, c.want)
			}
		})
	}
}

func TestWriteBackGitPinnedDigest(t *testing.T) {
	// setup commits files, with web on the digest strategy written back to
	// Git, and has the registry serve digestB for nginx:1, which Git pins to
	// digestA.
	setup := func(t *testing.T, files map[string]string) (*env, *fakeRegistry) {
		t.Helper()
		files["composecd/apps.yaml"] = strategyApp("web", "writeBack: git")
		e, reg := newWriteBackEnv(t, files)
		reg.set(nginx1, digestB)
		return e, reg
	}
	deployedFromGit := func(t *testing.T, e *env) {
		t.Helper()
		head := e.git("rev-parse", "HEAD")
		web := e.app("web")
		im := imageOf(web, "web")
		if !strings.Contains(e.fake().Running["web"], "image: nginx:1@"+digestB+"\n") || web.Sync.Status != Synced || web.Sync.Revision != head ||
			web.Diverged || im == nil || im.Pin != PinGit || im.Digest != digestB || im.UpdateAvailable || im.Diverged {
			t.Fatalf("web %+v, image %+v\n%s", web.Sync, im, e.fake().Running["web"])
		}
		if !e.c.ImageUpdateCommit(head) {
			t.Error("the commit isn't known as composecd's own")
		}
	}

	t.Run("a pin in the values is committed and deploys from Git", func(t *testing.T) {
		values := "# web\ntag: '1@" + digestA + "'   # pinned\n"
		e, _ := setup(t, map[string]string{"charts/web/values.yaml": values})
		e.refresh()
		if got, want := e.show("charts/web/values.yaml"), strings.Replace(values, digestA, digestB, 1); got != want {
			t.Errorf("values.yaml:\n%s\nwant:\n%s", got, want)
		}
		if msg := e.git("log", "-1", "--format=%B"); !strings.Contains(msg, "web/web: nginx 1@sha256:aaaaaaaaaaaa → 1@sha256:bbbbbbbbbbbb (charts/web/values.yaml tag)") {
			t.Errorf("commit message:\n%s", msg)
		}
		deployedFromGit(t, e)
	})

	t.Run("a digest in a key of its own is edited alone", func(t *testing.T) {
		e, _ := setup(t, map[string]string{
			"charts/web/templates/a.yaml": "services:\n  web:\n    image: nginx:{{ .Values.tag }}@{{ .Values.digest }}\n",
			"charts/web/values.yaml":      "tag: '1'\ndigest: " + digestA + "\n",
		})
		e.refresh()
		if got, want := e.show("charts/web/values.yaml"), "tag: '1'\ndigest: "+digestB+"\n"; got != want {
			t.Errorf("values.yaml = %q, want %q", got, want)
		}
		deployedFromGit(t, e)
	})

	t.Run("a pin committed to a branch for review deploys once merged", func(t *testing.T) {
		e, _ := setup(t, map[string]string{"charts/web/values.yaml": "tag: '1@" + digestA + "'\n"})
		e.c.Boot.ImageUpdates.WriteBack.Branch = "image-updates"
		e.refresh()
		commit := e.git("rev-parse", "image-updates")
		web := e.app("web")
		im := imageOf(web, "web")
		if im == nil || im.Commit != commit || !strings.HasPrefix(im.Note, "committed to Git as ") || im.Applicable || web.Sync.Status != Synced ||
			!strings.Contains(e.fake().Running["web"], "image: nginx:1@"+digestA+"\n") {
			t.Fatalf("image = %+v, sync %+v\n%s", im, web.Sync, e.fake().Running["web"])
		}
		e.checkImages("web")
		if e.git("rev-parse", "image-updates") != commit {
			t.Error("committed again")
		}
		e.git("merge", "--quiet", "--ff-only", "image-updates")
		e.refresh()
		deployedFromGit(t, e)
	})

	t.Run("a pin write-back can't edit is refused, not held", func(t *testing.T) {
		e, _ := setup(t, map[string]string{"charts/web/templates/a.yaml": "services:\n  web:\n    image: nginx:1@" + digestA + "\n"})
		e.refresh()
		head := e.git("rev-parse", "HEAD")
		e.refresh()
		web := e.app("web")
		im := imageOf(web, "web")
		if im == nil || im.Pin != PinGit || !im.UpdateAvailable || im.Applicable || im.Held != "" || im.Commit != "" ||
			!strings.HasPrefix(im.Note, gitPinHeldNote) || !strings.Contains(im.Note, "no values key sets the tag and digest of web") {
			t.Fatalf("image = %+v", im)
		}
		if web.Sync.Status != Synced || !strings.Contains(e.fake().Running["web"], "image: nginx:1@"+digestA+"\n") || e.git("rev-parse", "HEAD") != head {
			t.Fatalf("a refused update deployed or was committed: %+v\n%s", web.Sync, e.fake().Running["web"])
		}
		if _, err := e.c.DecideImage("web", "web", store.DecisionMemory, "trevor"); !errors.Is(err, ErrNotHeld) {
			t.Errorf("applying a Git pin in memory: %v", err)
		}
	})
}

func TestPinnedCandidate(t *testing.T) {
	for _, c := range []struct {
		image, latest, want string
	}{
		{"nginx:1@" + digestA, digestB, "1@" + digestB},
		{"nginx:1@" + digestA, digestA, ""},
		{"nginx:1@" + digestA, "", ""},
		{"nginx@" + digestA, digestB, ""},
		{"nginx:1", digestB, ""},
	} {
		svcs, err := images.Services("services:\n  web:\n    image: "+c.image+"\n", true, images.Policy{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := pinnedCandidate(svcs[0], c.latest); got != c.want {
			t.Errorf("pinnedCandidate(%s, %s) = %q, want %q", c.image, c.latest, got, c.want)
		}
	}
}
