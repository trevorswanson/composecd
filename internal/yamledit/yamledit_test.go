package yamledit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// edit is one Set on a document of a file.
type edit struct {
	doc   int
	key   string
	value string
}

func TestGolden(t *testing.T) {
	for name, edits := range map[string][]edit{
		"replace": {{key: "image.tag", value: "1.27.3"}, {key: "sidecar.tag", value: "1.37.0"}, {key: "worker.tag", value: "2.1.0"}},
		"unsafe":  {{key: "tag", value: "1.30"}},
		"add":     {{key: "apps.web.tag", value: "1.27.3"}},
		"addroot": {{key: "apps.web.tag", value: "1.27.3"}},
		"app":     {{doc: 0, key: "spec.source.values.tag", value: "1.27.3"}, {doc: 1, key: "spec.source.values.tag", value: "16.1"}},
	} {
		t.Run(name, func(t *testing.T) {
			data := read(t, name+".yaml")
			for _, e := range edits {
				docs, err := Docs(data)
				if err != nil {
					t.Fatal(err)
				}
				if data, err = Set(data, docs[e.doc], Key(e.key), e.value); err != nil {
					t.Fatalf("%s: %v", e.key, err)
				}
			}
			if want := string(read(t, name+".golden")); string(data) != want {
				t.Errorf("got:\n%s\nwant:\n%s", data, want)
			}
			// What was written reads back.
			docs, err := Docs(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range edits {
				if n := Find(docs[e.doc], Key(e.key)); n == nil || n.Value != e.value {
					t.Errorf("%s reads back as %+v", e.key, n)
				}
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	for name, c := range map[string]struct{ yaml, key, want string }{
		"alias":         {"base: &v 1.0\ntag: *v\n", "tag", "alias"},
		"anchor":        {"tag: &v 1.0\nother: *v\n", "tag", "anchor"},
		"block":         {"tag: |\n  1.0\n", "tag", "block scalar"},
		"tagged":        {"tag: !!str 1.0\n", "tag", "explicit tag"},
		"flow add":      {"apps: {web: {image: nginx}}\n", "apps.web.tag", "flow mapping"},
		"not a mapping": {"apps: [web]\n", "apps.web.tag", "not a mapping"},
		"not a scalar":  {"tag: {a: 1}\n", "tag", "not a scalar"},
	} {
		t.Run(name, func(t *testing.T) {
			root, err := Root([]byte(c.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Set([]byte(c.yaml), root, Key(c.key), "2.0"); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	if _, err := Root([]byte("a: 1\n---\nb: 2\n")); err == nil {
		t.Error("a values file of two documents was accepted")
	}
	if root, err := Root(nil); err != nil || root.Kind != yaml.MappingNode {
		t.Errorf("empty file = %+v, %v", root, err)
	}
	if out, err := Set(nil, &yaml.Node{Kind: yaml.MappingNode}, Key("tag"), "1"); err != nil || string(out) != "tag: \"1\"\n" {
		t.Errorf("empty file = %q, %v", out, err)
	}
}

func TestPlainSafe(t *testing.T) {
	for s, want := range map[string]bool{"1.27.3": true, "1.27": true, "1.30": false, "2": true, "v1.2": true, "latest": true,
		"yes": false, "on": false, "a: b": false, "x #y": false, "": false, "null": false, "~": false, "0x10": false} {
		if got := plainSafe(s); got != want {
			t.Errorf("plainSafe(%q) = %v, want %v", s, got, want)
		}
	}
}
