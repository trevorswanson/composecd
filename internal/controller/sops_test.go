package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/masking"
	"github.com/trevorswanson/composecd/internal/store"
)

type capturedNotifications struct{ events []Event }

func (n *capturedNotifications) Notify(e Event) { n.events = append(n.events, e) }

func controllerSOPS(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as fake sops")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
for arg in "$@"; do file="$arg"; done
if grep -q second "$file"; then password=second-secret; else password=first-secret; fi
printf 'password: %s\ntag: 1\n' "$password"
`
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("fake identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

const secretTemplate = `services:
  web:
    image: nginx:{{ .Values.tag }}
    environment: {PASSWORD: {{ .Values.password | quote }}}
  hook:
    image: busybox
    command: [echo, {{ .Values.password | quote }}]
    x-composetpl: {hook: post-install, delete-policy: hook-succeeded}
`

func TestSOPSDeployDiffAndHistoricalMasking(t *testing.T) {
	key := controllerSOPS(t)
	e := newEnv(t)
	e.c.Boot.SOPSAgeKeyFile = key
	notices := &capturedNotifications{}
	e.c.Notify = notices
	e.fake().RunOutput = "hook printed first-secret\n"
	e.commit(map[string]string{
		"composecd/apps.yaml":          appYAML("web", true, ", valueFiles: [secrets.sops.yaml]"),
		"charts/web/templates/a.yaml":  secretTemplate,
		"charts/web/secrets.sops.yaml": "password: ENC[first]\nsops: {age: []}\n",
	})
	e.refresh()
	if st := e.app("web"); st.Sync.Status != Synced || st.Error != "" {
		t.Fatalf("sync: %+v", st)
	}
	// Masking must never reach the real manifest or desired-state comparison.
	if !strings.Contains(e.fake().Running["web"], "first-secret") {
		t.Fatal("plaintext did not reach compose")
	}
	hist, err := e.c.Hosts.Get("nas").Store().History("web")
	if err != nil {
		t.Fatal(err)
	}
	if hist[0].Config["password"] != "first-secret" || !strings.Contains(hist[0].Manifest, "first-secret") {
		t.Fatal("host release values were redacted")
	}
	op, err := e.c.Store.LastOperation("web")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := e.c.Store.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted.Log, "first-secret") || !strings.Contains(persisted.Log, masking.Replacement) {
		t.Fatalf("SQLite log: %s", persisted.Log)
	}
	if err := e.c.Pause("web", "inspect secret rotation"); err != nil {
		t.Fatal(err)
	}
	e.commit(map[string]string{"charts/web/secrets.sops.yaml": "password: ENC[second]\nsops: {age: []}\n"})
	e.refresh()
	st := e.app("web")
	if st.Sync.Status != OutOfSync || len(st.Sync.Changes) == 0 {
		t.Fatalf("diff missing: %+v", st)
	}
	data, _ := json.Marshal(st)
	for _, secret := range []string{"first-secret", "second-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("status leaked", secret)
		}
	}
	if !strings.Contains(string(data), masking.Replacement) {
		t.Fatal("diff was not masked")
	}
	// Public getters return detached maps/slices, leaving internal state usable.
	st.Sync.Changes[0].Diff = "mutated"
	if e.app("web").Sync.Changes[0].Diff == "mutated" {
		t.Fatal("public diff aliases controller state")
	}
	if _, err := e.c.Sync("web", "operator"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if !strings.Contains(e.fake().Running["web"], "second-secret") {
		t.Fatal("secret rotation did not deploy")
	}
	// Simulate a restart: all historical user values are relearned even if the
	// new identity is unavailable. Rollback still uses the host's plain record.
	e.c.maskMu.Lock()
	e.c.secretValues = nil
	e.c.maskMu.Unlock()
	e.c.Boot.SOPSAgeKeyFile = filepath.Join(t.TempDir(), "missing")
	e.refresh()
	st = e.app("web")
	if st.Sync.Status != Unknown || !strings.Contains(st.Error, "secrets.sops.yaml") {
		t.Fatalf("missing key: %+v", st)
	}
	if got := e.c.Mask("web", "first-secret second-secret"); got != masking.Replacement+" "+masking.Replacement {
		t.Fatal("lost historical secrets", got)
	}
	if _, err := e.c.Rollback("web", 1, "operator"); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if !strings.Contains(e.fake().Running["web"], "first-secret") {
		t.Fatal("rollback redacted release values")
	}
	// Notifications and manually recorded failures are masked before dispatch,
	// logging and SQLite, including when their app has been removed from Git.
	testOp, err := e.c.startOperation("web", store.KindSync, store.TriggerManual, "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	testOp.Phase = store.PhaseFailed
	testOp.Log = "first-secret second-secret"
	testOp.Message = "failed: second-secret"
	e.c.finishOperation("web", testOp)
	for _, ev := range notices.events {
		if strings.Contains(ev.Message, "first-secret") || strings.Contains(ev.Message, "second-secret") {
			t.Fatal("notification leak", ev)
		}
	}
	e.c.mu.Lock()
	e.c.objects = &config.Objects{}
	e.c.mu.Unlock()
	if strings.Contains(e.c.Mask("web", "first-secret"), "first-secret") {
		t.Fatal("mask lost on removal")
	}
	stored, err := e.c.Store.Operation(testOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Log, "secret") || strings.Contains(stored.Message, "second-secret") {
		t.Fatal("persisted output leaked")
	}
}

func TestDecryptionFailureIsUnknown(t *testing.T) {
	key := controllerSOPS(t)
	e := newEnv(t)
	e.c.Boot.SOPSAgeKeyFile = key
	e.commit(map[string]string{"composecd/apps.yaml": appYAML("web", true, ", valueFiles: [accidental.sops.yaml]"), "charts/web/accidental.sops.yaml": "password: accidentally-plain\n"})
	e.refresh()
	st := e.app("web")
	if st.Sync.Status != Unknown || !strings.Contains(st.Error, "accidental.sops.yaml") || !strings.Contains(st.Error, "no SOPS metadata") {
		t.Fatalf("%+v", st)
	}
	if e.ups("web") != 0 {
		t.Fatal("deployed a misnamed plaintext values file")
	}
	// No decryption is done by the read-only getter.
	if _, err := e.c.App("missing"); err == nil {
		t.Fatal("unknown app accepted")
	}
}

func TestMaskingPreservesRouteIdentifiers(t *testing.T) {
	e := newEnv(t)
	e.refresh()
	e.c.remember("web", []string{"web", "web-web-1", "Synced", "Healthy"})
	st := e.app("web")
	if st.Name != "web" || st.Sync.Status != Synced || st.Health.Status != "Healthy" || st.Health.Containers[0].Name != "web-web-1" {
		t.Fatalf("masking broke routing/state: %+v", st)
	}
}
