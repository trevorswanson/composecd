package yamledit

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func str(s string) *yaml.Node     { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }
func boolean(b string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: b} }

func parse(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

const app = `# The media stack.
kind: Application
metadata:
  name: media # the release name
spec:
  source:
    path: charts/media
    valueFiles: [values.yaml]
  destination: {host: nas}
  syncPolicy:
    automated: {}
    retry:
      limit: 3
  dependsOn:
  # comments stay
  self: false
`

func TestSetValue(t *testing.T) {
	cases := []struct {
		name, path string
		v          *yaml.Node
		want       string
	}{
		{"scalar keeps the line comment", "metadata.name", str("films"), "metadata:\n  name: films # the release name\n"},
		{"typed scalar over a string", "spec.self", boolean("true"), "  self: true\n"},
		{"string that reads as a bool", "spec.source.path", str("true"), `    path: "true"` + "\n"},
		{"new key in a block mapping", "spec.syncPolicy.retry.backoff.duration", str("30s"),
			"    retry:\n      limit: 3\n      backoff:\n        duration: 30s\n  dependsOn:"},
		{"new key in an empty flow mapping", "spec.syncPolicy.automated.selfHeal", boolean("true"), "    automated: {selfHeal: true}\n"},
		{"inside a flow mapping", "spec.destination.host", str("pi"), "  destination: {host: pi}\n"},
		{"flow list keeps its style", "spec.source.valueFiles", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{str("a.yaml"), str("b.yaml")}},
			"    valueFiles: [a.yaml, b.yaml]\n"},
		{"empty value becomes a block list", "spec.dependsOn", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{str("traefik")}},
			"  dependsOn:\n    - traefik\n  # comments stay\n"},
		{"block value replaced", "spec.syncPolicy.retry", &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{str("limit"), {Kind: yaml.ScalarNode, Tag: "!!int", Value: "5"}}},
			"    retry:\n      limit: 5\n  dependsOn:"},
		{"new top-level section", "spec.permissions.env", &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle, Content: []*yaml.Node{str("*")}},
			"  self: false\n  permissions:\n    env: ['*']\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := parse(t, app)
			got, err := SetValue([]byte(app), doc.Content[0], Key(tc.path), tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if !contains(string(got), tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, got)
			}
			var check map[string]interface{}
			if err := yaml.Unmarshal(got, &check); err != nil {
				t.Errorf("doesn't parse: %v\n%s", err, got)
			}
			if !contains(string(got), "# The media stack.\n") {
				t.Error("lost the head comment")
			}
		})
	}
}

func TestDelete(t *testing.T) {
	cases := []struct{ path, want, gone string }{
		{"spec.syncPolicy.retry.limit", "    retry: {}\n", "limit: 3"},
		{"spec.syncPolicy.automated", "  syncPolicy:\n    retry:\n", "automated"},
		{"spec.destination.host", "  destination: {}\n", "host: nas"},
		{"spec.self", "  # comments stay\n", "self:"},
		{"spec.missing", "self: false", "missing"},
	}
	for _, tc := range cases {
		doc := parse(t, app)
		got, err := Delete([]byte(app), doc.Content[0], Key(tc.path))
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if !contains(string(got), tc.want) || contains(string(got), tc.gone) {
			t.Errorf("%s: got\n%s", tc.path, got)
		}
	}
}

func TestDocuments(t *testing.T) {
	data := []byte("kind: Host\nmetadata: {name: a}\n---\n# b\nkind: Host\nmetadata: {name: b}\n---\nkind: Host\nmetadata: {name: c}\n")
	docs, err := Docs(data)
	if err != nil {
		t.Fatal(err)
	}
	d := DocumentAt(data, docs[1])
	if got := string(data[d.Start:d.End]); got != "# b\nkind: Host\nmetadata: {name: b}\n" {
		t.Errorf("document: %q", got)
	}
	if got := string(ReplaceDocument(data, d, "kind: Host\nmetadata: {name: B}")); got != "kind: Host\nmetadata: {name: a}\n---\nkind: Host\nmetadata: {name: B}\n---\nkind: Host\nmetadata: {name: c}\n" {
		t.Errorf("replace: %q", got)
	}
	if got := string(RemoveDocument(data, d)); got != "kind: Host\nmetadata: {name: a}\n---\nkind: Host\nmetadata: {name: c}\n" {
		t.Errorf("remove middle: %q", got)
	}
	last := DocumentAt(data, docs[2])
	if got := string(RemoveDocument(data, last)); got != "kind: Host\nmetadata: {name: a}\n---\n# b\nkind: Host\nmetadata: {name: b}\n" {
		t.Errorf("remove last: %q", got)
	}
	if got := RemoveDocument([]byte("kind: Host\n"), Document{End: 11}); got != nil {
		t.Errorf("remove only: %q", got)
	}
	if got := string(AppendDocument([]byte("a: 1"), "b: 2")); got != "a: 1\n---\nb: 2\n" {
		t.Errorf("append: %q", got)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
