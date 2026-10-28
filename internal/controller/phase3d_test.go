package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/chart"
	"github.com/trevorswanson/composetpl/pkg/registry"
	"github.com/trevorswanson/composetpl/pkg/registry/registrytest"

	"github.com/trevorswanson/composecd/internal/gitcache"
)

func pushOCIChart(t *testing.T, client *registry.Client, version string) {
	t.Helper()
	archive, err := chart.WriteArchive("web", map[string][]byte{
		"Chart.yaml":         []byte("apiVersion: v2\nname: web\nversion: " + version + "\n"),
		"values.yaml":        []byte("tag: '1'\n"),
		"templates/web.yaml": []byte(webTemplate),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Push(context.Background(), archive, "oci://registry.lan/charts/"); err != nil {
		t.Fatal(err)
	}
}

func ociApp(name, valuesFrom string, auto bool, files string) string {
	s := "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n  source:\n    chart: oci://registry.lan/charts/web\n    version: ^1\n"
	if valuesFrom != "" {
		s += "    valuesFrom: " + valuesFrom + "\n    valueFiles: [" + files + "]\n"
	}
	s += "  destination: {host: nas}\n"
	if auto {
		s += "  syncPolicy: {automated: {}, options: {wait: false}}\n"
	}
	return s
}

func TestOCIRootValuesOnlyCommitsAutoSync(t *testing.T) {
	e := newEnv(t)
	e.c.Registry = registrytest.New().Client()
	pushOCIChart(t, e.c.Registry, "1.0.0")
	first := e.commit(map[string]string{"composecd/apps.yaml": ociApp("web", "{}", true, "values/web.yaml"), "values/web.yaml": "tag: '2'\n"})
	e.refresh()
	st := e.app("web")
	if st.Sync.Status != Synced || st.Sync.Revision != "" || st.Sync.ValuesRevision != first || st.Sync.DeployedValuesRevision != first || st.Release.Labels[LabelValuesRevision] != first {
		t.Fatalf("%+v", st)
	}
	if !strings.Contains(st.Sync.ChartRef, "@sha256:") || st.Release.Chart != "web-1.0.0" || st.Release.ChartRef != st.Sync.ChartRef || !strings.Contains(e.fake().Running["web"], "nginx:2") {
		t.Fatalf("pin/render: %+v", st)
	}
	e.refresh()
	if e.ups("web") != 1 {
		t.Fatal("unchanged values resynced")
	}
	// Commit metadata/unused values only: the rendered manifest is identical,
	// but the values commit is still an intentional new desired state.
	second := e.commit(map[string]string{"values/web.yaml": "tag: '2'\nnote: unreferenced\n"})
	e.refresh()
	if e.ups("web") != 2 || e.app("web").Sync.ValuesRevision != second {
		t.Fatalf("values-only commit did not sync: %+v", e.app("web"))
	}
	// A newer OCI version must also participate in desired-state identity, even
	// if its templates and rendered resources have not changed.
	pushOCIChart(t, e.c.Registry, "1.1.0")
	e.refresh()
	st = e.app("web")
	if e.ups("web") != 3 || st.Release.Chart != "web-1.1.0" {
		t.Fatalf("chart-only update: %+v", st)
	}
	// Removing valuesFrom clears its label and keeps existing inline-only OCI
	// sources working. Returning to a pinned values commit retains that checkout.
	e.commit(map[string]string{"composecd/apps.yaml": ociApp("web", "", true, "")})
	e.refresh()
	st = e.app("web")
	if st.Sync.Status != Synced || st.Sync.ValuesRevision != "" || st.Release.Labels[LabelValuesRevision] != "" {
		t.Fatalf("removed values source: %+v", st)
	}
	e.commit(map[string]string{"composecd/apps.yaml": ociApp("web", "{targetRevision: "+first+"}", true, "values/web.yaml")})
	e.refresh()
	st = e.app("web")
	if st.Sync.Status != Synced || st.Sync.ValuesRevision != first || st.Sync.DeployedValuesRevision != first {
		t.Fatalf("pinned values commit: %+v", st)
	}
	checkout := filepath.Join(e.c.Git.Dir, "checkouts", filepath.FromSlash(gitcache.CheckoutKey(e.repo, first)))
	if _, err := os.Stat(filepath.Join(checkout, "values", "web.yaml")); err != nil {
		t.Fatal("pinned values checkout pruned", err)
	}
}

func TestOCIExternalValuesFetchManualRefreshAndRetention(t *testing.T) {
	e := newEnv(t)
	values := &env{t: t, repo: t.TempDir()}
	values.git("init", "--quiet", "--initial-branch", "main")
	first := values.commit(map[string]string{"values/web.yaml": "tag: '3'\n"})
	e.c.Registry = registrytest.New().Client()
	pushOCIChart(t, e.c.Registry, "1.0.0")
	e.commit(map[string]string{"composecd/apps.yaml": ociApp("web", "{repoURL: "+values.repo+", targetRevision: main}", false, "values/web.yaml")})
	e.refresh()
	st := e.app("web")
	if st.Sync.ValuesRevision != first || st.Sync.Status != OutOfSync {
		t.Fatalf("%+v", st)
	}
	checkout := filepath.Join(e.c.Git.Dir, "checkouts", filepath.FromSlash(gitcache.CheckoutKey(values.repo, first)))
	if _, err := os.Stat(filepath.Join(checkout, "values", "web.yaml")); err != nil {
		t.Fatal("values checkout was pruned", err)
	}
	if !e.c.RefreshRepos([]string{values.repo + ".git"}) {
		t.Fatal("values webhook ignored")
	}
	<-e.c.refreshCh
	second := values.commit(map[string]string{"values/web.yaml": "tag: '4'\n"})
	// Sync must fetch values directly, without waiting for RefreshAll.
	if _, err := e.c.Sync("web", "operator"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	st = e.app("web")
	if st.Sync.ValuesRevision != second || st.Sync.DeployedValuesRevision != second || !strings.Contains(e.fake().Running["web"], "nginx:4") {
		t.Fatalf("manual sync: %+v", st)
	}
	third := values.commit(map[string]string{"values/web.yaml": "tag: '5'\n"})
	if err := e.c.RefreshApp("web"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	st = e.app("web")
	if st.Sync.ValuesRevision != third || st.Sync.DeployedValuesRevision != second || st.Sync.Status != OutOfSync {
		t.Fatalf("refresh: %+v", st)
	}
	e.c.pruneCheckouts(e.git("rev-parse", "HEAD"))
	old := filepath.Join(e.c.Git.Dir, "checkouts", filepath.FromSlash(gitcache.CheckoutKey(values.repo, first)))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("obsolete values checkout retained", err)
	}
}

func TestOCISOPSValuesAndMissingRepository(t *testing.T) {
	key := controllerSOPS(t)
	e := newEnv(t)
	e.c.Boot.SOPSAgeKeyFile = key
	e.c.Registry = registrytest.New().Client()
	pushOCIChart(t, e.c.Registry, "1.0.0")
	e.commit(map[string]string{"composecd/apps.yaml": ociApp("web", "{}", true, "values/secret.sops.yaml"), "values/secret.sops.yaml": "password: ENC[first]\nsops: {age: []}\n"})
	e.refresh()
	st := e.app("web")
	if st.Sync.Status != Synced || st.Sync.ValuesRevision == "" {
		t.Fatalf("%+v", st)
	}
	if got := e.c.Mask("web", "first-secret"); got == "first-secret" {
		t.Fatal("OCI SOPS leaves were not registered")
	}
	e.commit(map[string]string{"composecd/apps.yaml": ociApp("web", "{repoURL: "+filepath.Join(t.TempDir(), "absent")+"}", true, "values.yaml")})
	e.refresh()
	st = e.app("web")
	if st.Sync.Status != Unknown || !strings.Contains(st.Error, "not been fetched") {
		t.Fatalf("missing values repo: %+v", st)
	}
	if e.ups("web") != 1 {
		t.Fatal("deployed without values repository")
	}
}
