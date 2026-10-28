//go:build integration

package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

func TestIntegrationRealSOPSValues(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("SOPS not installed")
	}
	key, err := filepath.Abs("../../examples/sops/test-age-key.txt")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := os.ReadFile("../../examples/gitops/secrets/demo.sops.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	write(t, root, map[string]string{"chart/Chart.yaml": "name: c\n", "chart/values.sops.yaml": string(cipher), "chart/metadata.config": string(cipher)})
	for _, file := range []string{"values.sops.yaml", "metadata.config"} {
		a := app(config.AppSource{Path: "chart", ValueFiles: []string{file}})
		p, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, key)
		if err != nil || p.Values["password"] != "example-only-password" {
			t.Fatalf("%s: %v", file, err)
		}
		if len(p.Secrets) != 2 {
			t.Fatalf("unexpected secret leaves: %d", len(p.Secrets))
		}
		wrongKey := filepath.Join(t.TempDir(), "wrong-key")
		if err := os.WriteFile(wrongKey, []byte("this is not the encryption identity"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, wrongKey); err == nil || !strings.Contains(err.Error(), "decryption failed") {
			t.Fatalf("wrong key: %v", err)
		}
	}
}
