package config

import (
	"strings"
	"testing"
)

func TestOCIValuesFromDefaults(t *testing.T) {
	for _, tc := range []struct{ name, url, rev, wantURL, wantRev string }{
		{"root", "", "", "https://git.lan/root.git", "main"},
		{"explicit root", "https://git.lan/root.git", "", "https://git.lan/root.git", "main"},
		{"other repo", "https://git.lan/values.git", "", "https://git.lan/values.git", "HEAD"},
		{"pinned", "", "release", "https://git.lan/root.git", "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Application{Spec: AppSpec{Source: AppSource{Chart: "oci://registry.lan/charts/app", ValuesFrom: &GitSource{RepoURL: tc.url, TargetRevision: tc.rev}}}}
			o := &Objects{Applications: []*Application{a}}
			o.Resolve(Root{RepoURL: "https://git.lan/root.git", TargetRevision: "main"})
			s := a.Spec.Source
			if u, r := s.GitRepository(); u != tc.wantURL || r != tc.wantRev {
				t.Fatalf("%s %s", u, r)
			}
			if s.RepoURL != "" || s.TargetRevision != "" {
				t.Fatal("OCI chart gained a Git chart source")
			}
		})
	}
}
func TestOCIValuesValidation(t *testing.T) {
	host := "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {}\n---\n"
	for _, tc := range []struct {
		source string
		bad    bool
	}{
		{`{chart: oci://registry.lan/charts/app, valueFiles: [values/app.yaml]}`, true},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {}, valueFiles: [values/app.yaml]}`, false},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {repoURL: https://git.lan/values.git, targetRevision: prod}, valueFiles: [values/app.yaml]}`, false},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {}, valueFiles: [../outside.yaml]}`, true},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {}, valueFiles: [/etc/passwd]}`, true},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {}, valueFiles: ['C:/outside.yaml']}`, true},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {}, valueFiles: ['..\outside.yaml']}`, true},
		{`{path: charts/app, valuesFrom: {}}`, true},
		{`{path: charts/app, valueFiles: [../../values/app.yaml]}`, false},
		{`{chart: oci://registry.lan/charts/app, valuesFrom: {unknown: true}}`, true},
	} {
		doc := host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: app}\nspec:\n  source: " + tc.source + "\n  destination: {host: nas}\n"
		_, err := Parse("apps.yaml", []byte(doc))
		if (err != nil) != tc.bad {
			t.Errorf("%s: %v", tc.source, err)
		}
		if err != nil && !strings.Contains(err.Error(), "apps.yaml") {
			t.Fatal(err)
		}
	}
}
