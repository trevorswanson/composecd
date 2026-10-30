package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func monitoringFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{
		"objects/hosts.yaml": "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: ds9}\nspec: {}\n",
		"objects/apps.yaml": `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: dns}
spec:
  generators:
    - hosts: {}
  template:
    metadata:
      name: 'dns-{{ .host.name }}'
    spec:
      source:
        path: chart
        valueFiles: [override.yaml]
        infisical: {projectId: fixture, environment: prod, into: secrets}
        values:
          monitoring:
            group: inline
      destination:
        host: '{{ .host.name }}'
`,
		"chart/Chart.yaml":    "apiVersion: v2\nname: fixture\nversion: 0.1.0\n",
		"chart/values.yaml":   "password: DO_NOT_EXPORT\nmonitoring:\n  group: defaults\n  enabled: true\n  endpoints: [{name: dns, url: old, conditions: ['[DNS_RCODE] == NOERROR']}]\n",
		"chart/override.yaml": "monitoring:\n  group: override\n  endpoints: [{name: dns, url: '192.0.2.1', conditions: ['[DNS_RCODE] == NOERROR']}]\n",
	} {
		monitoringWrite(t, root, name, data)
	}
	return root
}

func monitoringWrite(t *testing.T, root, name, data string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func monitoringRun(root string) (string, error) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"monitoring", "export", filepath.Join(root, "objects"), "--repo-root", root})
	err := cmd.Execute()
	return out.String(), err
}

func TestMonitoringExportOfflinePrecedenceAndSecrets(t *testing.T) {
	root := monitoringFixture(t)
	t.Setenv("COMPOSECD_SERVER", "http://127.0.0.1:1")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	out, err := monitoringRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "DO_NOT_EXPORT") || strings.Contains(out, "infisical") || strings.Contains(out, "password") {
		t.Fatal("non-monitoring values leaked")
	}
	var result monitoringInventory
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || len(result.Applications) != 1 {
		t.Fatalf("inventory: %s", out)
	}
	app := result.Applications[0]
	m := app.Monitoring.(map[string]interface{})
	if app.Name != "dns-ds9" || app.Host != "ds9" || m["group"] != "inline" || m["enabled"] != true {
		t.Fatalf("wrong precedence: %s", out)
	}
	ep := m["endpoints"].([]interface{})[0].(map[string]interface{})
	if ep["url"] != "192.0.2.1" {
		t.Fatalf("value file not applied: %s", out)
	}
	again, err := monitoringRun(root)
	if err != nil || again != out {
		t.Fatal("nondeterministic export")
	}
}

func TestMonitoringExportFailsWithoutPartialOutput(t *testing.T) {
	for _, test := range []struct{ name, file, from, to string }{
		{"external repo", "objects/apps.yaml", "path: chart", "repoURL: https://example.invalid/private.git\n        path: chart"},
		{"explicit revision", "objects/apps.yaml", "path: chart", "path: chart\n        targetRevision: other"},
		{"secret overlap", "objects/apps.yaml", "into: secrets", "into: monitoring"},
		{"unscoped secrets", "objects/apps.yaml", ", into: secrets", ""},
		{"missing values", "objects/apps.yaml", "override.yaml", "missing.yaml"},
		{"traversal", "objects/apps.yaml", "override.yaml", "../../outside.yaml"},
		{"empty generator", "objects/apps.yaml", "hosts: {}", "hosts: {selector: {absent: yes}}"},
		{"bad yaml", "chart/override.yaml", "monitoring:", "monitoring: ["},
		{"sops", "chart/override.yaml", "monitoring:", "sops: {mac: ENC[fixture]}\nmonitoring:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := monitoringFixture(t)
			p := filepath.Join(root, test.file)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			monitoringWrite(t, root, test.file, strings.Replace(string(b), test.from, test.to, 1))
			out, err := monitoringRun(root)
			if err == nil || out != "" {
				t.Fatalf("want error and no output, got %q %v", out, err)
			}
		})
	}
}

func TestMonitoringExportRejectsSymlinks(t *testing.T) {
	for _, name := range []string{"chart/override.yaml", "objects/link.yaml"} {
		t.Run(name, func(t *testing.T) {
			root := monitoringFixture(t)
			outside := t.TempDir() + "/secret.yaml"
			if err := os.WriteFile(outside, []byte("monitoring: {secret: DO_NOT_EXPORT}"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, name)
			if name == "chart/override.yaml" {
				if err := os.Rename(target, target+".bak"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
			out, err := monitoringRun(root)
			if err == nil || out != "" {
				t.Fatalf("symlink accepted: %q %v", out, err)
			}
		})
	}
}
