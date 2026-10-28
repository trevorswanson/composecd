//go:build integration

package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/chart"
	"github.com/trevorswanson/composetpl/pkg/registry/registrytest"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/hosts"
)

func TestIntegrationContainerLogStream(t *testing.T) {
	for _, tc := range []struct {
		name           string
		encrypted, oci bool
	}{
		{"inline", false, false}, {"sops", true, false}, {"oci-values", false, true}, {"oci-sops-values", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) { integrationContainerLogStream(t, tc.encrypted, tc.oci) })
	}
}

func integrationContainerLogStream(t *testing.T, encrypted, oci bool) {
	for _, args := range [][]string{{"compose", "version"}, {"info"}} {
		if err := exec.Command("docker", args...).Run(); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatal("Docker with compose required:", err)
			}
			t.Skip("Docker with compose required:", err)
		}
	}
	keyFile := ""
	message := "integration-secret"
	if encrypted {
		if _, err := exec.LookPath("sops"); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatal("SOPS is required:", err)
			}
			t.Skip("SOPS is required:", err)
		}
		var err error
		keyFile, err = filepath.Abs("../../examples/sops/test-age-key.txt")
		if err != nil {
			t.Fatal(err)
		}
		message = "sops-example-message"
	}
	name := fmt.Sprintf("ccd-logs-%d", time.Now().UnixNano())
	repo, dir := t.TempDir(), t.TempDir()
	run := func(cmd string, args ...string) string {
		t.Helper()
		c := exec.Command(cmd, args...)
		c.Dir = repo
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v: %s", cmd, args, err, out)
		}
		return string(out)
	}
	run("git", "init", "--quiet", "--initial-branch=main")
	files := map[string]string{
		"composecd/hosts.yaml":            "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: local}\nspec: {storage: local, projectRoot: " + filepath.ToSlash(filepath.Join(dir, "projects")) + "}\n",
		"composecd/apps.yaml":             "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n  source: {path: charts/logs, values: {message: integration-secret}}\n  destination: {host: local}\n  syncPolicy: {automated: {}, options: {wait: false}}\n",
		"charts/logs/Chart.yaml":          "apiVersion: v2\nname: logs\nversion: 0.1.0\n",
		"charts/logs/templates/logs.yaml": "services:\n  box:\n    image: busybox:1.37\n    command: ['sh', '-c', 'echo stream-ready; echo {{ .Values.message }}; exec sleep 3600']\n",
	}
	if encrypted {
		cipher, err := os.ReadFile("../../examples/gitops/secrets/demo.sops.yaml")
		if err != nil {
			t.Fatal(err)
		}
		files["charts/logs/values.sops.yaml"] = string(cipher)
		files["composecd/apps.yaml"] = strings.Replace(files["composecd/apps.yaml"], "values: {message: integration-secret}", "valueFiles: [values.sops.yaml]", 1)
	}
	reg := registrytest.New().Client()
	if oci {
		archive, err := chart.WriteArchive("logs", map[string][]byte{
			"Chart.yaml":          []byte(files["charts/logs/Chart.yaml"]),
			"templates/logs.yaml": []byte(files["charts/logs/templates/logs.yaml"]),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := reg.Push(context.Background(), archive, "oci://registry.lan/charts/"); err != nil {
			t.Fatal(err)
		}
		file := "values/logs.yaml"
		files[file] = "message: integration-secret\n"
		if encrypted {
			file = "charts/logs/values.sops.yaml"
		}
		files["composecd/apps.yaml"] = "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n  source: {chart: 'oci://registry.lan/charts/logs', version: '^0.1', valuesFrom: {}, valueFiles: [" + file + "]}\n  destination: {host: local}\n  syncPolicy: {automated: {}, options: {wait: false}}\n"
	}
	for p, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "fixture")
	a := newAuthn(t, "secret")
	c := controller.New(controller.Options{Registry: reg, Boot: &config.Bootstrap{Concurrency: 1, SOPSAgeKeyFile: keyFile, Root: config.Root{RepoURL: repo, TargetRevision: "main", Path: "composecd"}}, Git: gitcache.New(filepath.Join(dir, "git"), nil), Hosts: hosts.NewRegistry(&hosts.CLI{DataDir: dir}), Store: a.Store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer func() {
		c.Close()
		c.Wait()
		ids, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+name).Output()
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
		_ = exec.Command("docker", "network", "rm", name+"_default").Run()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.RefreshAll(ctx)
	st, err := c.App(name)
	if err != nil || st.Error != "" || len(st.Health.Containers) == 0 {
		if st.Operation != nil {
			if op, e := c.Operation(st.Operation.ID); e == nil {
				t.Log(op.Log)
			}
		}
		t.Fatalf("app: %+v %v", st, err)
	}
	if oci {
		if st.Sync.Status != controller.Synced || !strings.Contains(st.Sync.ChartRef, "@sha256:") || st.Sync.ValuesRevision == "" || st.Sync.ValuesRevision != st.Sync.DeployedValuesRevision {
			t.Fatalf("OCI revisions: %+v", st.Sync)
		}
		if !encrypted {
			message = "integration-secret-v2"
			if err := os.WriteFile(filepath.Join(repo, "values", "logs.yaml"), []byte("message: "+message+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run("git", "add", ".")
			run("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "values-only update")
			commit := strings.TrimSpace(run("git", "rev-parse", "HEAD"))
			c.RefreshAll(ctx)
			st, err = c.App(name)
			if err != nil || st.Sync.Status != controller.Synced || st.Release == nil || st.Release.Revision != 2 || st.Sync.ValuesRevision != commit || st.Sync.DeployedValuesRevision != commit {
				t.Fatalf("values-only auto-sync: %+v %v", st, err)
			}
		}
	}
	rawLogs := ""
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
		rawLogs = run("docker", "logs", st.Health.Containers[0].Name)
		if strings.Contains(rawLogs, message) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(rawLogs, message) {
		t.Fatal("decrypted message never reached the real container")
	}
	if encrypted {
		hist, err := c.Hosts.Get("local").Store().History(name)
		if err != nil {
			t.Fatal(err)
		}
		if hist[0].Config["message"] != message {
			t.Fatal("host release lost its plaintext values")
		}
	}
	srv := httptest.NewServer(New(c, Options{Auth: a}))
	defer srv.Close()
	_, body := do(t, srv, "GET", "/api/v1/apps/"+name, "secret", "")
	if strings.Contains(body, message) || strings.Contains(body, "example-only-password") {
		t.Fatal("API leaked secret values")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/apps/"+name+"/containers/"+st.Health.Containers[0].Name+"/logs?follow=1&tail=200", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("logs: %d %s", resp.StatusCode, body)
	}
	scan := bufio.NewScanner(resp.Body)
	var output strings.Builder
	for scan.Scan() {
		output.WriteString(scan.Text())
		if strings.Contains(output.String(), "stream-ready") && strings.Contains(output.String(), "••••••") {
			break
		}
	}
	cancel()
	if strings.Contains(output.String(), message) || !strings.Contains(output.String(), "event: log") || !strings.Contains(output.String(), "••••••") {
		t.Fatalf("unmasked or missing logs: %s", &output)
	}
	// No other project or fabricated container name is accepted.
	code, _ := do(t, srv, "GET", "/api/v1/apps/"+name+"/containers/foreign/logs", "secret", "")
	if code != http.StatusNotFound {
		t.Fatal(code)
	}
}
