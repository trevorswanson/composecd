package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMonitoringDirectoryGeneratorAndExplicitApplication(t *testing.T) {
	root := monitoringFixture(t)
	monitoringWrite(t, root, "objects/apps.yaml", `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: directories}
spec:
  generators:
    - gitDirectories:
        directories: ['chart*']
        exclude: ['chart-skip']
  template:
    metadata:
      name: '{{ .path.basename }}'
    spec:
      source:
        path: '{{ .path.path }}'
      destination: {host: ds9}
---
apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: explicit}
spec:
  source: {path: chart, values: {monitoring: {enabled: false}}}
  destination: {host: ds9}
`)
	monitoringWrite(t, root, "chart-skip/Chart.yaml", "apiVersion: v2\nname: skipped\nversion: 0.1.0\n")
	out, err := monitoringRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"name": "chart"`) || !strings.Contains(out, `"name": "explicit"`) || strings.Contains(out, `chart-skip`) {
		t.Fatalf("wrong expansion: %s", out)
	}
	// A new matched values-only directory must not disappear silently.
	monitoringWrite(t, root, "chart-missing/values.yaml", "{}")
	out, err = monitoringRun(root)
	if err == nil || out != "" {
		t.Fatalf("skipped directory accepted: %s %v", out, err)
	}
}

func TestMonitoringRejectsDuplicateExpandedApplications(t *testing.T) {
	root := monitoringFixture(t)
	monitoringWrite(t, root, "objects/duplicate.yaml", `apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: dns-ds9}
spec:
  source: {path: chart}
  destination: {host: ds9}
`)
	out, err := monitoringRun(root)
	if err == nil || out != "" {
		t.Fatalf("duplicate accepted: %s %v", out, err)
	}
}

func TestMonitoringRejectsObjectsDirectorySymlinkEscape(t *testing.T) {
	root := monitoringFixture(t)
	outside := t.TempDir()
	if err := os.Rename(filepath.Join(root, "objects"), filepath.Join(outside, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "objects"), filepath.Join(root, "objects")); err != nil {
		t.Fatal(err)
	}
	out, err := monitoringRun(root)
	if err == nil || out != "" {
		t.Fatalf("object directory escape accepted: %s %v", out, err)
	}
}
