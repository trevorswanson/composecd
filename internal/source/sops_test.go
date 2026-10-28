package source

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

// A tiny fake checks the exact CLI contract, returns plaintext in chunks and
// deliberately leaks a value on stderr on failure; no diagnostics may expose it.
func fakeSOPS(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake sops is a shell script")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$1 $2 $3" = "--decrypt --output-type yaml" ] || exit 7
[ -n "$SOPS_AGE_KEY_FILE" ] || exit 8
[ -z "$SOPS_AGE_KEY_CMD" ] || exit 9
if [ "$SOPS_TEST_FAILURE" = yes ]; then echo 'private-secret stderr' >&2; echo 'private-secret stdout'; exit 1; fi
printf '%s\n' 'password: private-secret' 'nested: {number: 12345, short: abc}' 'items: [another-secret, null]' 'tag: decrypted'
`
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOPS_AGE_KEY_CMD", "must-not-execute")
	key := filepath.Join(t.TempDir(), "age-key")
	if err := os.WriteFile(key, []byte("test identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestPrepareSOPS(t *testing.T) {
	key := fakeSOPS(t)
	root := t.TempDir()
	encrypted := "password: ENC[data]\nsops: {age: [{recipient: age1test}]}\n"
	write(t, root, map[string]string{"chart/Chart.yaml": "name: c\n", "chart/plain.yaml": "tag: plain\nnested: {retained: yes}\n", "chart/secrets.sops.yaml": encrypted, "chart/metadata.yaml": encrypted, "chart/last.yaml": "tag: last\n"})
	for _, name := range []string{"secrets.sops.yaml", "metadata.yaml"} {
		a := app(config.AppSource{Path: "chart", ValueFiles: []string{"plain.yaml", name, "last.yaml"}, Values: map[string]interface{}{"tag": "inline"}})
		p, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, key)
		if err != nil {
			t.Fatal(err)
		}
		if p.Values["password"] != "private-secret" || p.Values["tag"] != "inline" {
			t.Fatalf("merge: %#v", p.Values)
		}
		if !reflect.DeepEqual(p.Values["nested"], map[string]interface{}{"number": float64(12345), "short": "abc", "retained": true}) {
			t.Fatalf("nested: %#v", p.Values)
		}
		for _, secret := range []string{"private-secret", "another-secret", "12345", "decrypted"} {
			found := false
			for _, s := range p.Secrets {
				found = found || s == secret
			}
			if !found {
				t.Errorf("missing secret %s: %v", secret, p.Secrets)
			}
		}
		for _, s := range p.Secrets {
			if s == "abc" {
				t.Fatal("short value masked")
			}
		}
		raw, _ := os.ReadFile(filepath.Join(root, "chart", name))
		if string(raw) != encrypted {
			t.Fatal("checkout modified")
		}
	}
}

func TestPrepareSOPSErrors(t *testing.T) {
	key := fakeSOPS(t)
	root := t.TempDir()
	write(t, root, map[string]string{"chart/Chart.yaml": "name: c\n", "chart/secrets.sops.yaml": "password: ENC[data]\nsops: {age: []}\n", "chart/plain.sops.yml": "password: plaintext-secret\n"})
	a := app(config.AppSource{Path: "chart", ValueFiles: []string{"secrets.sops.yaml"}})
	for _, k := range []string{"", filepath.Join(t.TempDir(), "missing-key"), t.TempDir()} {
		_, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, k)
		if err == nil || !strings.Contains(err.Error(), "secrets.sops.yaml") || !strings.Contains(err.Error(), "sopsAgeKeyFile") {
			t.Fatalf("key %s: %v", k, err)
		}
	}
	t.Setenv("SOPS_TEST_FAILURE", "yes")
	_, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, key)
	if err == nil || !strings.Contains(err.Error(), "secrets.sops.yaml") || !strings.Contains(err.Error(), "decryption failed") || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("decryption error: %v", err)
	}
	a.Spec.Source.ValueFiles = []string{"plain.sops.yml"}
	_, err = Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, key)
	if err == nil || !strings.Contains(err.Error(), "no SOPS metadata") || strings.Contains(err.Error(), "plaintext-secret") {
		t.Fatal(err)
	}
}

func TestValidateSecretsWithSiblingCharts(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{".git": "worktree marker", ".sops.yaml": "creation_rules: []\n", "composecd/marker": "", "charts/app/Chart.yaml": "name: app\n", "charts/app/plain.sops.yaml": "password: accidentally-plain\n"})
	objs := &config.Objects{Applications: []*config.Application{app(config.AppSource{Path: "charts/app", ValueFiles: []string{"plain.sops.yaml"}})}}
	if err := ValidateSecrets(filepath.Join(root, "composecd"), objs); err == nil || !strings.Contains(err.Error(), "no SOPS metadata") {
		t.Fatal(err)
	}
	write(t, root, map[string]string{"charts/app/plain.sops.yaml": "sops: {age: []}\npassword: ENC[data]\n"})
	if err := ValidateSecrets(filepath.Join(root, "composecd"), objs); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSecrets(root, objs); err != nil {
		t.Fatal(err)
	}
}
