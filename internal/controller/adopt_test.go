package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"
	"sigs.k8s.io/yaml"

	"github.com/trevorswanson/composecd/internal/config"
)

func TestUnmanagedAndAdoption(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	h := e.c.Hosts.Get("nas")
	saved := &release.Release{Name: "existing", Revision: 2, Status: release.StatusDeployed, ProjectDir: h.ProjectDir("existing"),
		Chart:  release.ChartInfo{Ref: filepath.Join(e.repo, "charts/web"), Version: "0.1.0"},
		Config: map[string]interface{}{"tag": "2", "password": "plaintext-secret", "nested": map[string]interface{}{"enabled": true, "count": float64(2)}}}
	pending := &release.Release{Name: "pending", Revision: 1, Status: release.StatusPendingInstall}
	removed := &release.Release{Name: "removed", Revision: 1, Status: release.StatusUninstalled}
	orphan := &release.Release{Name: "orphan", Revision: 1, Status: release.StatusDeployed, Labels: map[string]string{LabelManagedBy: ManagedBy}, Chart: release.ChartInfo{Ref: "oci://registry.lan/box@sha256:" + strings.Repeat("a", 64)}}
	// Save externally after the host cache was loaded, as composetpl would.
	if err := e.backend.Store(h).Save(saved, pending, removed, orphan); err != nil {
		t.Fatal(err)
	}
	f := e.fake()
	f.Running["existing"] = "services: {}"
	f.Running["plain"] = "services: {}"
	f.Containers = map[string][]compose.Container{"plain": {{Labels: compose.Labels{"com.docker.compose.project.config_files": "/srv/plain/compose.yaml,/srv/plain/override.yaml"}}}}
	out, err := e.c.Unmanaged(context.Background(), "nas")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Releases) != 3 || out.Releases[0].Name != "existing" || !out.Releases[1].Orphaned || out.Releases[2].Blocked == "" {
		t.Fatalf("%+v", out)
	}
	if len(out.Projects) != 1 || out.Projects[0].Name != "plain" || len(out.Projects[0].ConfigFiles) != 2 || !strings.Contains(out.Projects[0].Hint, "composetpl create") {
		t.Fatalf("%+v", out.Projects)
	}
	inventory, _ := json.Marshal(out)
	if strings.Contains(string(inventory), "plaintext-secret") {
		t.Fatal("discovery leaked saved values")
	}
	before := f.CallLog()
	adoption, err := e.c.Adopt(context.Background(), "nas", "existing")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse("adopted", []byte(hostsYAML+"---\n"+adoption.YAML))
	if err != nil {
		t.Fatal(err, adoption.YAML)
	}
	app := parsed.Application("existing")
	gotValues, _ := json.Marshal(app.Spec.Source.Values)
	wantValues, _ := json.Marshal(saved.Config)
	if app == nil || app.Spec.Destination.Host != "nas" || app.Spec.Source.Path != "charts/web" || app.Spec.SyncPolicy.Automated != nil || string(gotValues) != string(wantValues) {
		t.Fatalf("%+v", app)
	}
	if len(adoption.Warnings) == 0 || f.CallLog() != before {
		t.Fatal("export must warn without changing Docker")
	}
	after, _ := storage.Latest(e.backend.Store(h), "existing")
	if after.Revision != 2 || len(after.Labels) > 0 {
		t.Fatal("export modified release", after)
	}
	oci, err := e.c.Adopt(context.Background(), "nas", "orphan")
	if err != nil || !strings.Contains(oci.YAML, orphan.Chart.Ref) {
		t.Fatal(oci, err)
	}
	for _, tc := range []struct {
		host, name string
		want       error
	}{{"absent", "existing", ErrNotFound}, {"nas", "absent", ErrNotFound}, {"nas", "removed", ErrNotFound}, {"nas", "pending", ErrAdoptionBlocked}, {"nas", "web", ErrAdoptionBlocked}, {"nas", "plain", ErrNotFound}} {
		if _, err := e.c.Adopt(context.Background(), tc.host, tc.name); !errors.Is(err, tc.want) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if _, err := e.c.Unmanaged(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Committing the exported YAML is still manual, then sync upgrades revision 2
	// instead of installing another project. Strip extra test values from render.
	e.commit(map[string]string{"composecd/adopted.yaml": adoption.YAML})
	e.refresh()
	if s := e.app("existing"); s.Release.Revision != 2 || s.Sync.Status != OutOfSync {
		t.Fatalf("%+v", s)
	}
	if _, err := e.c.Sync("existing", "operator"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if s := e.app("existing"); s.Release.Revision != 3 || s.Release.Labels[LabelManagedBy] != ManagedBy || s.Sync.Status != Synced {
		t.Fatalf("%+v", s)
	}
}

func TestAdoptionChartPathsAndWarnings(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	ctx := context.Background()
	checkout, err := e.c.Git.Checkout(ctx, e.repo, e.c.Root().Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"charts/web", filepath.Join(checkout, "charts/web"), filepath.Join(e.repo, "charts/web")} {
		if got := e.c.adoptionChartPath(ctx, ref); got != "charts/web" {
			t.Errorf("%q => %q", ref, got)
		}
	}
	for _, ref := range []string{"../charts/web", "", "missing", filepath.Join(t.TempDir(), "charts/web"), `C:\outside\web`} {
		if got := e.c.adoptionChartPath(ctx, ref); got != "" {
			t.Errorf("unsafe path %q => %q", ref, got)
		}
	}
	// The symlink itself can be in the checkout, but its target must stay there.
	if err := os.Symlink(e.repo, filepath.Join(checkout, "outside")); err == nil {
		if got := e.c.adoptionChartPath(ctx, "outside/charts/web"); got != "" {
			t.Fatal(got)
		}
	}
	h := e.c.Hosts.Get("nas")
	if err := h.Store().Save(&release.Release{Name: "external", Revision: 1, Status: release.StatusDeployed, Chart: release.ChartInfo{Ref: "/elsewhere/web"}, ProjectDir: "/old/external"}); err != nil {
		t.Fatal(err)
	}
	out, err := e.c.Adopt(ctx, "nas", "external")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(out.YAML), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.YAML, "charts/external") || !strings.Contains(strings.Join(out.Warnings, " "), "projectRoot") || !strings.Contains(strings.Join(out.Warnings, " "), "Copy it") {
		t.Fatal(out)
	}
}

func TestAdoptionBareOCIUsesRecordedVersion(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	h := e.c.Hosts.Get("nas")
	if err := h.Store().Save(&release.Release{Name: "oci", Revision: 1, Status: release.StatusDeployed, Chart: release.ChartInfo{Ref: "oci://registry.lan/charts/web", Version: "1.2.3"}}); err != nil {
		t.Fatal(err)
	}
	out, err := e.c.Adopt(context.Background(), "nas", "oci")
	if err != nil || !strings.Contains(out.YAML, "version: 1.2.3") {
		t.Fatal(out, err)
	}
}
