package config

import (
	"strings"
	"testing"
)

const setYAML = `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: stacks}
spec:
  generators:
    - gitDirectories:
        directories: ['charts/*']
  template:
    metadata: {name: '{{ .path.basename }}'}
    spec:
      source: {path: '{{ .path.path }}'}
      destination: {host: nas}
`

func TestApplicationSetDefinitionsAndDeferredReferences(t *testing.T) {
	doc := "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {}\n---\n" + setYAML + "---\napiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: explicit}\nspec: {source: {path: charts/static}, destination: {host: nas}, dependsOn: [generated]}\n"
	o, err := Parse("apps.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(o.ApplicationSets) != 1 || o.ApplicationSets[0].Origin != "apps.yaml:6" {
		t.Fatalf("%+v", o.ApplicationSets)
	}
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "generated") {
		t.Fatal("full validation allowed an unresolved reference", err)
	}
	o.Resolve(Root{RepoURL: "https://git.lan/root.git", TargetRevision: "main"})
	g := o.ApplicationSets[0].Spec.Generators[0].GitDirectories
	if g.RepoURL != "https://git.lan/root.git" || g.TargetRevision != "main" {
		t.Fatal(g)
	}
	o.ApplicationSets[0].Spec.Generators[0].GitDirectories.RepoURL = "https://other/repo.git"
	g.TargetRevision = ""
	o.Resolve(Root{RepoURL: "https://git.lan/root.git", TargetRevision: "main"})
	if g.TargetRevision != "HEAD" {
		t.Fatal(g)
	}
}
func TestInvalidApplicationSetDefinitions(t *testing.T) {
	for _, doc := range []string{
		strings.Replace(setYAML, "name: stacks", "name: Bad_Name", 1),
		setYAML + "---\n" + setYAML,
		strings.Replace(setYAML, "    - gitDirectories:\n        directories: ['charts/*']", "    - list: []", 1),
		strings.Replace(setYAML, "    - gitDirectories:\n        directories: ['charts/*']", "    - {}", 1),
		strings.Replace(setYAML, "    - gitDirectories:\n        directories: ['charts/*']", "    - hosts: {}\n      gitDirectories: {directories: ['charts/*']}", 1),
		strings.Replace(setYAML, "['charts/*']", "[]", 1),
		strings.Replace(setYAML, "['charts/*']", "['../escape/*']", 1),
		strings.Replace(setYAML, "['charts/*']", "['[bad']", 1),
		strings.Replace(setYAML, "['charts/*']", "['/absolute/*']", 1),
		strings.Replace(setYAML, "metadata: {name: '{{ .path.basename }}'}", "metadata: {}", 1),
		strings.Replace(setYAML, "      source:", "      unknown:", 1),
	} {
		if _, err := Parse("sets.yaml", []byte(doc)); err == nil {
			t.Errorf("accepted invalid set: %s", doc)
		}
	}
}

func TestApplicationSetStrategy(t *testing.T) {
	strategy := func(s string) string {
		return strings.Replace(setYAML, "  template:", "  strategy:\n"+s+"  template:", 1)
	}
	o, err := Parse("sets.yaml", []byte(strategy(`    type: RollingSync
    rollingSync:
      maxUpdate: 50%
      minHealthyTime: 30s
      steps:
        - matchExpressions: [{key: tier, operator: In, values: [canary]}]
          maxUpdate: 1
        - matchExpressions: [{key: tier, operator: DoesNotExist}]
`)))
	if err != nil {
		t.Fatal(err)
	}
	rs := o.ApplicationSets[0].Spec.Strategy.RollingSync
	if *rs.MaxUpdate != (MaxUpdate{Value: 50, Percent: true}) || rs.Steps[0].MaxUpdate.Of(5) != 1 || rs.Steps[1].MaxUpdate != nil {
		t.Fatalf("%+v", rs)
	}
	if !rs.Steps[0].Selects(map[string]string{"tier": "canary"}) || rs.Steps[0].Selects(map[string]string{}) ||
		!rs.Steps[1].Selects(map[string]string{"other": "x"}) || rs.Steps[1].Selects(map[string]string{"tier": "canary"}) {
		t.Error("step selection")
	}
	for _, c := range []struct {
		m    *MaxUpdate
		n, k int
	}{{nil, 4, 4}, {&MaxUpdate{Value: 2}, 4, 2}, {&MaxUpdate{Value: 0}, 4, 0}, {&MaxUpdate{Value: 50, Percent: true}, 3, 1},
		{&MaxUpdate{Value: 10, Percent: true}, 3, 1}, {&MaxUpdate{Value: 0, Percent: true}, 3, 0}} {
		if got := c.m.Of(c.n); got != c.k {
			t.Errorf("%v of %d = %d, want %d", c.m, c.n, got, c.k)
		}
	}
	for _, s := range []string{
		"    type: Canary\n    rollingSync: {}\n",
		"    type: RollingSync\n",
		"    type: RollingSync\n    rollingSync: {maxUpdate: -1}\n",
		"    type: RollingSync\n    rollingSync: {maxUpdate: 101%}\n",
		"    type: RollingSync\n    rollingSync: {maxUpdate: one}\n",
		"    type: RollingSync\n    rollingSync: {minHealthyTime: soon}\n",
		"    type: RollingSync\n    rollingSync: {steps: [{matchExpressions: [{key: a, operator: In}]}]}\n",
		"    type: RollingSync\n    rollingSync: {steps: [{matchExpressions: [{key: a, operator: Exists, values: [b]}]}]}\n",
		"    type: RollingSync\n    rollingSync: {steps: [{matchExpressions: [{key: a, operator: Gt, values: ['1']}]}]}\n",
		"    type: RollingSync\n    rollingSync: {steps: [{matchExpressions: [{operator: Exists}]}]}\n",
		"    type: RollingSync\n    rollingSync: {concurrency: 1}\n",
	} {
		if _, err := Parse("sets.yaml", []byte(strategy(s))); err == nil {
			t.Errorf("accepted invalid strategy:\n%s", s)
		}
	}
}
