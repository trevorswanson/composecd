package appset

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func baseSet() *config.ApplicationSet {
	return &config.ApplicationSet{Metadata: config.Metadata{Name: "stacks"}, Spec: config.ApplicationSetSpec{Template: config.Application{Metadata: config.Metadata{Name: "{{ .path.basename | lower }}"}, Spec: config.AppSpec{Source: config.AppSource{Path: "{{ .path.path }}"}, Destination: config.Destination{Host: "nas"}}}}}
}
func TestGitDirectoryParametersAndStringRendering(t *testing.T) {
	root := fixture(t, map[string]string{"charts/Web/Chart.yaml": "name: web", "charts/db/Chart.yaml": "name: db", "charts/old-app/Chart.yaml": "name: old", "charts/not-a-chart/readme": "no", "charts/nested/child/Chart.yaml": "name: child", ".hidden/app/Chart.yaml": "name: hidden"})
	set := baseSet()
	set.Spec.Generators = []config.Generator{{GitDirectories: &config.GitDirectories{RepoURL: "https://git.lan/charts.git", TargetRevision: "prod", Directories: []string{"charts/*", "charts/Web"}, Exclude: []string{"charts/old-*"}}}}
	set.Spec.Template.Metadata.Labels = map[string]string{"segment": "{{ index .path.segments 0 }}", "composecd/applicationset": "cannot-override"}
	set.Spec.Template.Spec.Source.Values = map[string]interface{}{"{{ .path.basename }}": []interface{}{"{{ .path.path | upper }}", true, float64(42)}, "quoted": "a \"quote\"\n{{ .path.basename }}"}
	released := 0
	apps, skipped, err := Generate(context.Background(), set, nil, func(_ context.Context, url, revision string) (string, func(), error) {
		if url != "https://git.lan/charts.git" || revision != "prod" {
			t.Fatal(url, revision)
		}
		return root, func() { released++ }, nil
	})
	if err != nil || len(apps) != 2 || released != 1 {
		t.Fatalf("%v %v %d", apps, err, released)
	}
	// Matched directories without a Chart.yaml are reported, not silently dropped.
	if !reflect.DeepEqual(skipped, []string{"charts/nested", "charts/not-a-chart"}) {
		t.Fatal(skipped)
	}
	if apps[0].Metadata.Name != "db" || apps[1].Metadata.Name != "web" {
		t.Fatal(apps)
	}
	a := apps[1]
	if a.Origin != "applicationset stacks (charts/Web)" || a.Metadata.Labels[config.LabelApplicationSet] != "stacks" || a.Metadata.Labels["segment"] != "charts" {
		t.Fatalf("%+v", a)
	}
	if a.Spec.Source.RepoURL != "https://git.lan/charts.git" || a.Spec.Source.TargetRevision != "prod" || a.Spec.Source.Path != "charts/Web" {
		t.Fatal(a.Spec.Source)
	}
	if !reflect.DeepEqual(a.Spec.Source.Values["Web"], []interface{}{"CHARTS/WEB", true, float64(42)}) || a.Spec.Source.Values["quoted"] != "a \"quote\"\nWeb" {
		t.Fatal(a.Spec.Source.Values)
	}
	if set.Spec.Template.Metadata.Name != "{{ .path.basename | lower }}" || set.Spec.Template.Metadata.Labels[config.LabelApplicationSet] != "cannot-override" {
		t.Fatal("template was mutated")
	}
}
func TestHostsSelectorsAndIndependentGenerators(t *testing.T) {
	hosts := []*config.Host{{Metadata: config.Metadata{Name: "nas", Labels: map[string]string{"role": "server"}}}, {Metadata: config.Metadata{Name: "pi", Labels: map[string]string{"role": "edge"}}}, {Metadata: config.Metadata{Name: "other"}}}
	set := baseSet()
	set.Spec.Template.Metadata.Name = "agent-{{ .host.name }}"
	set.Spec.Template.Spec.Source.Path = "charts/agent"
	set.Spec.Template.Spec.Destination.Host = "{{ .host.name }}"
	set.Spec.Template.Spec.Source.Env = map[string]string{"ROLE": "{{ .host.labels.role | upper }}"}
	set.Spec.Generators = []config.Generator{{Hosts: &config.HostsGenerator{Selector: map[string]string{"role": "server"}}}, {Hosts: &config.HostsGenerator{Selector: map[string]string{"role": "edge"}}}}
	apps, _, err := Generate(context.Background(), set, hosts, nil)
	if err != nil || len(apps) != 2 {
		t.Fatalf("%v %v", apps, err)
	}
	if apps[0].Metadata.Name != "agent-nas" || apps[1].Metadata.Name != "agent-pi" || apps[0].Spec.Destination.Host != "nas" || apps[0].Spec.Source.Env["ROLE"] != "SERVER" {
		t.Fatal(apps)
	}
	set.Spec.Generators = []config.Generator{{Hosts: &config.HostsGenerator{Selector: map[string]string{"missing": ""}}}}
	if apps, _, err := Generate(context.Background(), set, hosts, nil); err != nil || len(apps) != 0 {
		t.Fatalf("missing label matched: %v %v", apps, err)
	}
}
func TestTemplateErrorsAndUnavailableHelpers(t *testing.T) {
	set := baseSet()
	set.Spec.Generators = []config.Generator{{Hosts: &config.HostsGenerator{}}}
	for _, text := range []string{"{{ .path.basename }}", "{{ env \"HOME\" }}", "{{ expandenv \"$HOME\" }}", "{{ getHostByName \"localhost\" }}", "{{ randAlpha 5 }}", "{{ now }}", "{{", "{{ .host.labels.missing }}"} {
		set.Spec.Template.Metadata.Name = text
		if apps, _, err := Generate(context.Background(), set, []*config.Host{{Metadata: config.Metadata{Name: "nas"}}}, nil); err == nil || apps != nil {
			t.Fatalf("%q: %v %v", text, apps, err)
		}
	}
	set.Spec.Template.Metadata.Name = "{{ .host.name }}"
	set.Spec.Template.Metadata.Labels = map[string]string{"{{ .host.name }}": "a", "nas": "b"}
	if _, _, err := Generate(context.Background(), set, []*config.Host{{Metadata: config.Metadata{Name: "nas"}}}, nil); err == nil || !strings.Contains(err.Error(), "duplicate rendered map key") {
		t.Fatal(err)
	}
}
