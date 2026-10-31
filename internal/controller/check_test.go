package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/source"
)

// checkRepo writes files into a fake checkout and loads its composecd/ objects.
func checkRepo(t *testing.T, files map[string]string) (string, *config.Objects) {
	t.Helper()
	root := t.TempDir()
	files[".git/HEAD"] = "ref: refs/heads/main\n"
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "composecd")
	objs, err := config.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, objs
}

const checkHosts = `apiVersion: composecd/v1alpha1
kind: Host
metadata: {name: a}
---
apiVersion: composecd/v1alpha1
kind: Host
metadata: {name: b}
`

func checkResult(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", name, results)
	return CheckResult{}
}

func TestCheckRendersAppSetsAndLookups(t *testing.T) {
	dir, objs := checkRepo(t, map[string]string{
		"composecd/hosts.yaml": checkHosts,
		"composecd/apps.yaml": `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: web}
spec:
  generators: [{hosts: {}}]
  template:
    metadata: {name: "web-{{ .host.name }}"}
    spec:
      source: {path: charts/web, values: {host: "{{ .host.name }}"}}
      destination: {host: "{{ .host.name }}"}
---
apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: monitor}
spec:
  source: {path: charts/monitor}
  destination: {host: a}
`,
		"charts/web/Chart.yaml":           "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		"charts/web/templates/web.yaml":   "services:\n  web:\n    image: nginx\n    labels:\n      check.url: http://{{ .Values.host }}\n",
		"charts/monitor/Chart.yaml":       "apiVersion: v2\nname: monitor\nversion: 0.1.0\n",
		"charts/monitor/templates/m.yaml": "services:\n  m:\n    image: busybox\n    command:\n{{- range (lookup \"composecd/application\" \"\").items }}\n      - {{ index .services.web.labels \"check.url\" }}\n{{- end }}\n",
	})
	results, sets, err := Check(context.Background(), dir, objs, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].Err != nil || strings.Join(sets[0].Applications, ",") != "web-a,web-b" {
		t.Fatalf("sets: %+v", sets)
	}
	if len(results) != 3 {
		t.Fatalf("results: %+v", results)
	}
	m := checkResult(t, results, "monitor")
	if m.Err != nil || !strings.Contains(m.Manifest, "- http://a\n") || !strings.Contains(m.Manifest, "- http://b\n") {
		t.Fatalf("monitor: %v\n%s", m.Err, m.Manifest)
	}
	if !strings.HasPrefix(checkResult(t, results, "web-b").Manifest, "name: web-b\n") {
		t.Fatal("manifest isn't named after the application")
	}
}

func TestCheckReportsBrokenCharts(t *testing.T) {
	dir, objs := checkRepo(t, map[string]string{
		"composecd/hosts.yaml": checkHosts,
		"composecd/apps.yaml": `apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: broken}
spec:
  source: {path: charts/broken}
  destination: {host: a}
---
apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: elsewhere}
spec:
  source: {repoURL: https://example.com/other.git, path: x}
  destination: {host: b}
---
apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: nochart}
spec:
  source: {path: charts/missing}
  destination: {host: b}
`,
		"charts/broken/Chart.yaml":          "apiVersion: v2\nname: broken\nversion: 0.1.0\n",
		"charts/broken/templates/fail.yaml": "{{ fail \"label is malformed\" }}\n",
	})
	results, _, err := Check(context.Background(), dir, objs, CheckOptions{RepoURL: "ssh://git@git.example.com:2222/me/apps"})
	if err != nil {
		t.Fatal(err)
	}
	if r := checkResult(t, results, "broken"); r.Err == nil || !strings.Contains(r.Err.Error(), "label is malformed") {
		t.Fatalf("broken: %v", r.Err)
	}
	if r := checkResult(t, results, "elsewhere"); r.Skipped == "" || r.Err != nil {
		t.Fatalf("elsewhere: %+v", r)
	}
	if r := checkResult(t, results, "nochart"); r.Err == nil || !strings.Contains(r.Err.Error(), "not found") {
		t.Fatalf("nochart: %v", r.Err)
	}
}

func TestCheckStubsSecrets(t *testing.T) {
	dir, objs := checkRepo(t, map[string]string{
		"composecd/hosts.yaml": checkHosts,
		"composecd/app.yaml": `apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: db}
spec:
  source:
    path: charts/db
    valueFiles: [secrets.sops.yaml]
    infisical: {projectId: p, environment: prod, into: extra}
  destination: {host: a}
`,
		"charts/db/Chart.yaml":        "apiVersion: v2\nname: db\nversion: 0.1.0\n",
		"charts/db/secrets.sops.yaml": "db:\n  password: ENC[AES256_GCM,data:x]\nsops: {age: [{recipient: age1x}]}\n",
		"charts/db/templates/db.yaml": "services:\n  db:\n    image: postgres\n    environment:\n      PASSWORD: {{ .Values.db.password }}\n      EXTRA: {{ len (.Values.extra | default dict) | quote }}\n",
	})
	results, _, err := Check(context.Background(), dir, objs, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := checkResult(t, results, "db")
	if r.Err != nil || !strings.Contains(r.Manifest, "PASSWORD: "+source.Placeholder) {
		t.Fatalf("db: %v\n%s", r.Err, r.Manifest)
	}
	if strings.Join(r.Stubbed, "; ") != `valueFiles "secrets.sops.yaml"; source.infisical` {
		t.Fatalf("stubbed: %q", r.Stubbed)
	}
}

func TestCheckReadersOfEachOther(t *testing.T) {
	reader := "services:\n  s:\n    image: busybox\n    labels:\n      seen: {{ len (lookup \"composecd/application\" \"\").items | quote }}\n"
	dir, objs := checkRepo(t, map[string]string{
		"composecd/hosts.yaml": checkHosts,
		"composecd/apps.yaml": `apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: one}
spec: {source: {path: charts/r}, destination: {host: a}}
---
apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: two}
spec: {source: {path: charts/r}, destination: {host: b}}
`,
		"charts/r/Chart.yaml":       "apiVersion: v2\nname: r\nversion: 0.1.0\n",
		"charts/r/templates/r.yaml": reader,
	})
	results, _, err := Check(context.Background(), dir, objs, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	one, two := checkResult(t, results, "one"), checkResult(t, results, "two")
	if one.Err != nil || two.Err != nil {
		t.Fatalf("errors: %v, %v", one.Err, two.Err)
	}
	// The first to render can't see the other; the second sees the first.
	if len(one.Notes) != 1 || !strings.Contains(one.Manifest, `seen: "0"`) || !strings.Contains(two.Manifest, `seen: "1"`) {
		t.Fatalf("one: %v\n%s\ntwo: %v\n%s", one.Notes, one.Manifest, two.Notes, two.Manifest)
	}
}

func TestSameRepo(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://git.example.com/me/apps.git", "ssh://git@git.example.com:2222/me/apps", true},
		{"git@GitHub.com:me/apps.git", "https://github.com/me/apps/", true},
		{"https://user:token@git.example.com/me/apps", "https://git.example.com/me/apps", true},
		{"https://git.example.com/me/apps", "https://git.example.com/me/other", false},
		{"", "", false},
	} {
		if got := sameRepo(c.a, c.b); got != c.want {
			t.Errorf("sameRepo(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
