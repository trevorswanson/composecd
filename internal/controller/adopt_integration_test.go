//go:build integration

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
)

func TestIntegrationAdoptExistingRelease(t *testing.T) {
	e, name := newDockerEnv(t, config.StorageLocal)
	e.refresh()
	h := e.c.Hosts.Get("local")
	// Install through the actual composetpl CLI, outside composecd, preserving
	// the eventual project directory so adoption need not recreate the container.
	composetpl(t, []string{"COMPOSETPL_STORAGE=local", "COMPOSETPL_STATE_DIR=" + releasesDir(e)},
		"install", name, filepath.Join(e.repo, "charts/box"), "--set", "message=adopted", "--project-directory", h.ProjectDir(name), "--wait", "--timeout", "2m")
	before := docker(t, "inspect", "--format", "{{.Id}}", name+"-box-1")
	inventory, err := e.c.Unmanaged(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range inventory.Releases {
		if r.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatal("external install not discovered", inventory)
	}
	// Discover a plain Compose project and its original config file paths too.
	plain := name + "-plain"
	t.Cleanup(func() { removeProject(plain) })
	configFile := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(configFile, []byte("services:\n  plain:\n    image: busybox:1.37\n    command: [sleep, '86400']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docker(t, "compose", "-p", plain, "-f", configFile, "up", "-d")
	inventory, err = e.c.Unmanaged(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, p := range inventory.Projects {
		if p.Name == plain {
			found = true
			if len(p.ConfigFiles) != 1 || p.ConfigFiles[0] != configFile {
				t.Fatalf("config paths: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("plain project not discovered", inventory)
	}
	adoption, err := e.c.Adopt(context.Background(), "local", name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(adoption.YAML, "path: charts/box") || !strings.Contains(adoption.YAML, "message: adopted") {
		t.Fatal(adoption)
	}
	rel, err := storage.Latest(h.Store(), name)
	if err != nil || rel.Revision != 1 || rel.Labels[LabelManagedBy] != "" {
		t.Fatal(rel, err)
	}
	e.commit(map[string]string{"composecd/apps.yaml": adoption.YAML})
	e.refresh()
	if st := e.app(name); st.Release.Revision != 1 || st.Sync.Status != OutOfSync || !strings.Contains(strings.Join(st.Sync.Reasons, " "), "not yet managed") {
		t.Fatalf("before sync: %+v", st)
	}
	if _, err := e.c.Sync(name, "operator"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	st := e.app(name)
	if st.Release.Revision != 2 || st.Release.Labels[LabelManagedBy] != ManagedBy || st.Sync.Status != Synced {
		t.Fatalf("after sync: %+v", st)
	}
	if got := docker(t, "inspect", "--format", "{{.Id}}", name+"-box-1"); got != before {
		t.Fatalf("unchanged service was recreated: %s => %s", before, got)
	}
	if got := docker(t, "exec", name+"-box-1", "cat", "/etc/greeting"); got != "adopted" {
		t.Fatal(got)
	}
	inventory, err = e.c.Unmanaged(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range inventory.Releases {
		if r.Name == name {
			t.Fatal("adopted app still unmanaged")
		}
	}
}
