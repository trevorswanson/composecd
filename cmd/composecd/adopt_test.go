package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
)

func TestAdoptCLI(t *testing.T) {
	srv, c, _ := startServer(t)
	h := c.Hosts.Get("nas")
	if err := h.Store().Save(&release.Release{Name: "external", Revision: 1, Status: release.StatusDeployed,
		Chart: release.ChartInfo{Ref: filepath.Join(c.Boot.Root.RepoURL, "charts/whoami")}, Config: map[string]interface{}{"password": "secret-export"}}); err != nil {
		t.Fatal(err)
	}
	out := mustCLI(t, srv, "host", "unmanaged", "nas")
	expect(t, out, "external", "release", "composecd adopt nas external")
	if strings.Contains(out, "secret-export") {
		t.Fatal("inventory leaked values")
	}
	out = mustCLI(t, srv, "host", "unmanaged", "nas", "-o", "json")
	expect(t, out, `"releases"`, `"name": "external"`)
	cmd := newRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "test-token-0123456789", "adopt", "nas", "external"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	expect(t, stdout.String(), "apiVersion: composecd/v1alpha1", "kind: Application", "name: external", "host: nas", "path: charts/whoami", "secret-export")
	if strings.Contains(stdout.String(), "Warning:") || !strings.Contains(stderr.String(), "plaintext") {
		t.Fatal(stdout.String(), stderr.String())
	}
	// Parse the command's stdout as standalone YAML, without diagnostics mixed in.
	if _, err := config.Parse("adopted", []byte("apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {}\n---\n"+stdout.String())); err != nil {
		t.Fatal(err)
	}
	rel, err := storage.Latest(h.Store(), "external")
	if err != nil || rel.Revision != 1 || len(rel.Labels) > 0 {
		t.Fatal(rel, err)
	}
	if _, err := cli(t, srv, "adopt", "nas", "missing"); err == nil {
		t.Fatal("missing release accepted")
	}
}
