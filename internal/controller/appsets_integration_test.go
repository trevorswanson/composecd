//go:build integration

package controller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

func TestIntegrationApplicationSetGenerationAndPrune(t *testing.T) {
	e, name := newDockerEnv(t, config.StorageLocal)
	first, second := name+"-box", name+"-other"
	t.Cleanup(func() { e.c.Close(); e.c.Wait(); removeProject(first); removeProject(second) })
	set := fmt.Sprintf(`apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: ci-stacks}
spec:
  generators:
    - gitDirectories: {directories: ['charts/box', 'charts/other']}
  template:
    metadata: {name: '%s-{{ .path.basename }}'}
    spec:
      source: {path: '{{ .path.path }}'}
      destination: {host: local}
      syncPolicy: {automated: {prune: true}, options: {timeout: 2m}}
`, name)
	e.commit(map[string]string{
		"composecd/apps.yaml":             "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec: {source: {path: charts/other}, destination: {host: local}}\n",
		"composecd/sets.yaml":             set,
		"charts/other/Chart.yaml":         itChart,
		"charts/other/values.yaml":        "message: generated\n",
		"charts/other/templates/box.yaml": itTemplate,
	})
	e.refresh()
	status := e.c.ApplicationSets()
	if len(status) != 1 || !status[0].Ready || status[0].Generated != 2 {
		t.Fatalf("%+v root=%s", status, e.c.Root().Error)
	}
	for _, app := range []string{first, second} {
		st := e.app(app)
		if st.Sync.Status != Synced || st.Release.Labels[LabelApplicationSet] != "ci-stacks" {
			t.Fatalf("%+v", st)
		}
		if got := containers(t, app); got != app+"-box-1 running" {
			t.Fatalf("containers: %s", got)
		}
	}
	e.git("rm", "-r", "charts/box")
	e.commit(nil)
	e.refresh()
	if got := containers(t, first); got != "" {
		t.Fatal("removed directory wasn't pruned", got)
	}
	if got := containers(t, second); got == "" {
		t.Fatal("remaining app was pruned")
	}
	if st := e.c.ApplicationSets()[0]; st.Generated != 1 || !st.Ready {
		t.Fatalf("%+v", st)
	}
	// A remaining explicit app prevents the whole-root guard from hiding a
	// broken per-set zero guard. Its chart path is allowed to become unavailable.
	e.git("rm", "-r", "charts/other")
	e.commit(nil)
	e.refresh()
	st := e.c.ApplicationSets()[0]
	if st.Generated != 0 || st.Ready || !strings.Contains(st.Error, "generated: 0, was: 1") {
		t.Fatalf("%+v", st)
	}
	if got := containers(t, second); got == "" {
		t.Fatal("empty generator pruned the final real container")
	}
}
