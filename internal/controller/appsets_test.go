package controller

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/compose/composetest"
	"github.com/trevorswanson/composetpl/pkg/release"
)

func directorySet(name, glob string) string {
	return fmt.Sprintf(`apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: %s}
spec:
  generators:
    - gitDirectories:
        directories: [%q]
  template:
    metadata: {name: 'gen-{{ .path.basename }}'}
    spec:
      source: {path: '{{ .path.path }}'}
      destination: {host: nas}
      syncPolicy: {automated: {prune: true}, options: {wait: false}}
`, name, glob)
}
func generatedFixture(e *env) {
	e.t.Helper()
	e.commit(map[string]string{
		"composecd/apps.yaml":             appYAML("manual", false, ""),
		"composecd/sets.yaml":             directorySet("stacks", "charts/app-*"),
		"charts/app-a/Chart.yaml":         "name: app-a\nversion: 0.1.0\n",
		"charts/app-a/values.yaml":        "tag: '1'\n",
		"charts/app-a/templates/web.yaml": webTemplate,
		"charts/app-b/Chart.yaml":         "name: app-b\nversion: 0.1.0\n",
		"charts/app-b/values.yaml":        "tag: '1'\n",
		"charts/app-b/templates/web.yaml": webTemplate,
	})
	e.refresh()
}

func TestApplicationSetGenerationAndCrossDependencies(t *testing.T) {
	e := newEnv(t)
	generatedFixture(e)
	sets := e.c.ApplicationSets()
	if len(sets) != 1 || !sets[0].Ready || sets[0].Generated != 2 || strings.Join(sets[0].Applications, ",") != "gen-app-a,gen-app-b" {
		t.Fatalf("%+v", sets)
	}
	st := e.app("gen-app-a")
	if st.Sync.Status != Synced || st.Origin != "applicationset stacks (charts/app-a)" || st.Labels[LabelApplicationSet] != "stacks" {
		t.Fatalf("%+v", st)
	}
	rel, err := e.c.Hosts.Get("nas").Store().Get("gen-app-a", 1)
	if err != nil || rel.Labels[LabelApplicationSet] != "stacks" {
		t.Fatalf("%v %v", rel, err)
	}
	sets[0].Applications[0] = "mutated"
	if e.c.ApplicationSets()[0].Applications[0] == "mutated" {
		t.Fatal("status aliases controller state")
	}
	// Explicit applications may depend on generated applications.
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("manual", true, "") + "  dependsOn: [gen-app-a]\n"})
	e.refresh()
	if st := e.app("manual"); st.Sync.Status != Synced || st.Blocked != "" {
		t.Fatalf("cross dependency: %+v", st)
	}
	// Generated applications may depend on explicit applications too.
	set := strings.Replace(directorySet("stacks", "charts/app-*"), "      source:", "      dependsOn: [manual]\n      source:", 1)
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("manual", true, ""), "composecd/sets.yaml": set})
	e.refresh()
	if !e.c.ApplicationSets()[0].Ready {
		t.Fatal(e.c.Root().Error)
	}
	// A cycle across generated and explicit apps rejects the whole new snapshot.
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("manual", true, "") + "  dependsOn: [gen-app-a]\n"})
	e.refresh()
	if !strings.Contains(e.c.Root().Error, "cycle") || e.c.ApplicationSets()[0].Ready {
		t.Fatal(e.c.Root().Error)
	}
}

func TestApplicationSetDirectoryRemovalAndZeroGuard(t *testing.T) {
	e := newEnv(t)
	generatedFixture(e)
	e.git("rm", "-r", "charts/app-a")
	e.commit(nil)
	e.refresh()
	if _, err := e.c.App("gen-app-a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, ok := e.fake().Running["gen-app-a"]; ok {
		t.Fatal("removed directory was not pruned")
	}
	hist, err := e.c.Hosts.Get("nas").Store().History("gen-app-a")
	if err != nil || hist[len(hist)-1].Status != release.StatusUninstalled {
		t.Fatalf("%+v %v", hist, err)
	}
	e.git("rm", "-r", "charts/app-b")
	e.commit(nil)
	e.refresh()
	sets := e.c.ApplicationSets()
	if len(sets) != 1 || sets[0].Generated != 0 || sets[0].Previous != 1 || sets[0].Ready || !strings.Contains(sets[0].Error, "generated: 0, was: 1") {
		t.Fatalf("%+v", sets)
	}
	if _, ok := e.fake().Running["gen-app-b"]; !ok {
		t.Fatal("empty generator pruned existing release")
	}
	if _, err := e.c.App("gen-app-b"); err != nil {
		t.Fatal("lost last good application", err)
	}
	// Simulate restart: forget generated objects/status, retaining the same host
	// release store. The explicit app means the whole-root empty guard won't help.
	e.c.mu.Lock()
	e.c.objects = nil
	e.c.apps = map[string]*appState{}
	e.c.appsets = nil
	e.c.mu.Unlock()
	e.refresh()
	sets = e.c.ApplicationSets()
	if sets[0].Previous != 1 || sets[0].Ready || !strings.Contains(sets[0].Error, "protecting existing releases") {
		t.Fatalf("restart guard: %+v", sets)
	}
	if _, ok := e.fake().Running["gen-app-b"]; !ok {
		t.Fatal("restart bypassed the empty-generator guard")
	}
	hs := e.c.HostStatuses()[0]
	if strings.Join(hs.Orphans, ",") != "gen-app-b" {
		t.Fatalf("protected orphan: %+v", hs)
	}
}

func TestApplicationSetTemplateErrorsAndCollisions(t *testing.T) {
	for _, mode := range []string{"missing parameter", "explicit collision", "within set collision", "between sets collision"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			generatedFixture(e)
			set := directorySet("stacks", "charts/app-*")
			files := map[string]string{}
			switch mode {
			case "missing parameter":
				set = strings.Replace(set, "gen-{{ .path.basename }}", "{{ .missing }}", 1)
			case "explicit collision":
				set = strings.Replace(set, "gen-{{ .path.basename }}", "manual", 1)
			case "within set collision":
				set = strings.Replace(set, "gen-{{ .path.basename }}", "duplicate", 1)
			case "between sets collision":
				files["composecd/another.yaml"] = directorySet("other", "charts/app-*")
			}
			files["composecd/sets.yaml"] = set
			e.commit(files)
			e.refresh()
			if e.c.Root().Error == "" {
				t.Fatal("invalid generation accepted")
			}
			sets := e.c.ApplicationSets()
			for _, s := range sets {
				if s.Ready || s.Error == "" {
					t.Fatalf("error not reported: %+v", s)
				}
			}
			if _, ok := e.fake().Running["gen-app-a"]; !ok {
				t.Fatal("failed generation pruned last good apps")
			}
			if len(e.c.Apps()) != 3 {
				t.Fatalf("partial snapshot applied: %+v", e.c.Apps())
			}
		})
	}
}

func TestExternalGitDirectoryGenerator(t *testing.T) {
	e := newEnv(t)
	external := &env{t: t, repo: t.TempDir()}
	external.git("init", "--quiet", "--initial-branch", "main")
	external.commit(map[string]string{"charts/external/Chart.yaml": "name: external\nversion: 0.1.0\n", "charts/external/values.yaml": "tag: '7'\n", "charts/external/templates/web.yaml": webTemplate})
	set := strings.Replace(directorySet("external", "charts/*"), "        directories:", fmt.Sprintf("        repoURL: %q\n        targetRevision: main\n        directories:", external.repo), 1)
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("manual", false, ""), "composecd/sets.yaml": set})
	e.refresh()
	st := e.app("gen-external")
	if st.Sync.Status != Synced || st.Source.RepoURL != external.repo || !strings.Contains(e.fake().Running["gen-external"], "nginx:7") {
		t.Fatalf("%+v", st)
	}
	if !e.c.RefreshRepos([]string{external.repo + ".git"}) {
		t.Fatal("generator repo webhook ignored")
	}
	<-e.c.refreshCh
	external.commit(map[string]string{"charts/added/Chart.yaml": "name: added\nversion: 0.1.0\n", "charts/added/values.yaml": "tag: '8'\n", "charts/added/templates/web.yaml": webTemplate})
	e.refresh()
	if e.c.ApplicationSets()[0].Generated != 2 || e.app("gen-added").Sync.Status != Synced {
		t.Fatal(e.c.ApplicationSets())
	}
}

const hostsSet = `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: agents}
spec:
  generators:
    - hosts: {selector: {role: server}}
  template:
    metadata: {name: 'agent-{{ .host.name }}'}
    spec:
      source: {path: charts/web}
      destination: {host: '{{ .host.name }}'}
      syncPolicy: {automated: {prune: true}, options: {wait: false}}
`

func TestHostGeneratorRemovalPrunesRetiredHost(t *testing.T) {
	e := newEnv(t)
	e.backend.fakes["worker"] = composetest.New()
	hosts := hostsYAML + "---\napiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: worker, labels: {role: server}}\nspec: {}\n"
	hosts = strings.Replace(hosts, "metadata: {name: nas}", "metadata: {name: nas, labels: {role: server}}", 1)
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("manual", false, ""), "composecd/sets.yaml": hostsSet, "composecd/hosts.yaml": hosts})
	e.refresh()
	if e.c.ApplicationSets()[0].Generated != 2 || e.app("agent-worker").Sync.Status != Synced {
		t.Fatal(e.c.ApplicationSets())
	}
	// Keep one matching host so this is a real removal rather than a zero-result
	// set; the removed engine's last connection is retained long enough to prune.
	e.commit(map[string]string{"composecd/hosts.yaml": strings.Replace(hostsYAML, "metadata: {name: nas}", "metadata: {name: nas, labels: {role: server}}", 1)})
	e.refresh()
	if _, ok := e.backend.fakes["worker"].Running["agent-worker"]; ok {
		t.Fatal("removed host's generated app was not pruned")
	}
	if e.c.Hosts.Get("worker") != nil {
		t.Fatal("host still active")
	}
	found := false
	for _, h := range e.c.HostStatuses() {
		if h.Name == "worker" {
			found = h.Retired
		}
	}
	if !found {
		t.Fatal("retired host not visible")
	}
	// Removing the final selector match triggers the zero guard and retains nas.
	e.commit(map[string]string{"composecd/hosts.yaml": hostsYAML})
	e.refresh()
	if e.c.ApplicationSets()[0].Generated != 0 || !strings.Contains(e.c.ApplicationSets()[0].Error, "was: 1") {
		t.Fatal(e.c.ApplicationSets())
	}
	if _, ok := e.fake().Running["agent-nas"]; !ok {
		t.Fatal("empty host selector pruned final app")
	}
}

func TestGeneratedOwnershipAndPrunePolicyUpdatesSync(t *testing.T) {
	e := newEnv(t)
	generatedFixture(e)
	set := strings.Replace(directorySet("stacks", "charts/app-*"), "name: stacks", "name: renamed", 1)
	e.commit(map[string]string{"composecd/sets.yaml": set})
	e.refresh()
	rel, err := e.c.Hosts.Get("nas").Store().Get("gen-app-a", 2)
	if err != nil || rel.Labels[LabelApplicationSet] != "renamed" || e.app("gen-app-a").Sync.Status != Synced {
		t.Fatalf("ownership not synced: %+v %v", rel, err)
	}
	set = strings.Replace(set, "prune: true", "prune: false", 1)
	e.commit(map[string]string{"composecd/sets.yaml": set})
	e.refresh()
	rel, err = e.c.Hosts.Get("nas").Store().Get("gen-app-a", 3)
	if err != nil || rel.Labels[LabelPrune] != "" {
		t.Fatalf("prune policy not synced: %+v %v", rel, err)
	}
	e.git("rm", "-r", "charts/app-a")
	e.commit(nil)
	e.refresh()
	if _, ok := e.fake().Running["gen-app-a"]; !ok {
		t.Fatal("app was pruned after template disabled prune")
	}
}

// A broken set keeps its last good apps and doesn't hold back the rest of Git.
func TestApplicationSetErrorIsIsolated(t *testing.T) {
	e := newEnv(t)
	generatedFixture(e)
	broken := strings.Replace(directorySet("stacks", "charts/app-*"), "gen-{{ .path.basename }}", "{{ .missing }}", 1)
	e.commit(map[string]string{
		"composecd/sets.yaml": broken,
		"composecd/apps.yaml": appYAML("manual", false, "") + "---\n" + appYAML("added", false, ""),
	})
	e.refresh()
	if _, err := e.c.App("added"); err != nil {
		t.Fatal("a broken set held back an explicit app:", err)
	}
	for _, name := range []string{"gen-app-a", "gen-app-b"} {
		if _, err := e.c.App(name); err != nil {
			t.Fatal("lost last good application", name, err)
		}
		if _, ok := e.fake().Running[name]; !ok {
			t.Fatal("a broken set pruned", name)
		}
	}
	sets := e.c.ApplicationSets()
	if len(sets) != 1 || sets[0].Ready || !strings.Contains(sets[0].Error, "missing") || strings.Join(sets[0].Applications, ",") != "gen-app-a,gen-app-b" {
		t.Fatalf("%+v", sets)
	}
	if !strings.Contains(e.c.Root().Error, "applicationset stacks") {
		t.Fatal("set error not reported on the root:", e.c.Root().Error)
	}
	// After a restart there is nothing to keep, and the guard protects the releases.
	e.c.mu.Lock()
	e.c.objects = nil
	e.c.appsets = nil
	e.c.mu.Unlock()
	e.refresh()
	if _, ok := e.fake().Running["gen-app-a"]; !ok {
		t.Fatal("restart pruned a broken set's releases")
	}
	if _, err := e.c.App("added"); err != nil {
		t.Fatal(err)
	}
}

// A generated app whose chart doesn't render must not leave its set looking
// healthy: the set reports the app's error, named by repository path.
func TestApplicationSetReportsAppRenderErrors(t *testing.T) {
	e := newEnv(t)
	generatedFixture(e)
	if sets := e.c.ApplicationSets(); len(sets[0].AppErrors) != 0 || len(sets[0].Warnings) != 0 {
		t.Fatalf("%+v", sets)
	}
	e.commit(map[string]string{"charts/app-b/Chart.yaml": "name: [oops\n"})
	e.refresh()
	sets := e.c.ApplicationSets()
	// composetpl joins the chart directory with OS separators.
	got := filepath.ToSlash(sets[0].AppErrors["gen-app-b"])
	if !sets[0].Ready || len(sets[0].AppErrors) != 1 || !strings.Contains(got, "rendering: ") || !strings.Contains(got, "charts/app-b/Chart.yaml") {
		t.Fatalf("%+v", sets)
	}
	if strings.Contains(got, "checkouts") || strings.Contains(e.app("gen-app-b").Error, "checkouts") {
		t.Fatalf("error leaks the controller's checkout path: %s", got)
	}
	e.commit(map[string]string{"charts/app-b/Chart.yaml": "name: app-b\nversion: 0.1.0\n"})
	e.refresh()
	if sets := e.c.ApplicationSets(); len(sets[0].AppErrors) != 0 {
		t.Fatalf("%+v", sets)
	}
}

// Directories that match but hold no chart, and sets that generate nothing,
// are reported instead of silently producing no applications.
func TestApplicationSetWarnsAboutSkippedDirectories(t *testing.T) {
	e := newEnv(t)
	e.commit(map[string]string{
		"composecd/sets.yaml":             directorySet("stacks", "charts/app-*"),
		"charts/app-a/Chart.yml":          "name: app-a\nversion: 0.1.0\n",
		"charts/app-a/templates/web.yaml": webTemplate,
	})
	e.refresh()
	st := e.c.ApplicationSets()[0]
	if !st.Ready || st.Generated != 0 || strings.Join(st.Warnings, "|") != "generated no applications|skipped 1 matched directory with no Chart.yaml at the generated source path: charts/app-a" {
		t.Fatalf("%+v", st)
	}
	e.git("mv", "charts/app-a/Chart.yml", "charts/app-a/Chart.yaml")
	e.commit(map[string]string{"charts/app-a/values.yaml": "tag: '1'\n"})
	e.refresh()
	if st := e.c.ApplicationSets()[0]; st.Generated != 1 || len(st.Warnings) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestSetWarningsBoundsListedDirectories(t *testing.T) {
	var skipped []string
	for i := 0; i < maxSkippedListed+2; i++ {
		skipped = append(skipped, fmt.Sprintf("d%02d", i))
	}
	w := setWarnings(1, skipped)
	if len(w) != 1 || !strings.HasPrefix(w[0], "skipped 12 matched directories") || !strings.HasSuffix(w[0], "d09, and 2 more") {
		t.Fatal(w)
	}
}

// Directories holding only values deploy a chart from elsewhere in the repository.
func TestApplicationSetValuesOnlyDirectories(t *testing.T) {
	e := newEnv(t)
	set := strings.Replace(directorySet("envs", "envs/*"), "      source: {path: '{{ .path.path }}'}",
		"      source: {path: charts/web, valueFiles: ['../../{{ .path.path }}/values.yaml']}", 1)
	e.commit(map[string]string{
		"composecd/sets.yaml":    set,
		"envs/prod/values.yaml":  "tag: prod\n",
		"envs/stage/values.yaml": "tag: stage\n",
	})
	e.refresh()
	st := e.c.ApplicationSets()[0]
	if !st.Ready || st.Generated != 2 || len(st.Warnings) != 0 || len(st.AppErrors) != 0 {
		t.Fatalf("%+v", st)
	}
	for _, env := range []string{"prod", "stage"} {
		if a := e.app("gen-" + env); a.Sync.Status != Synced || a.Error != "" {
			t.Fatalf("%+v", a)
		}
		rel, err := e.c.Hosts.Get("nas").Store().Get("gen-"+env, 1)
		if err != nil || !strings.Contains(rel.Manifest, "image: nginx:"+env) {
			t.Fatalf("%v %v", rel, err)
		}
	}
}
