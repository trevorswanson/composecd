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
