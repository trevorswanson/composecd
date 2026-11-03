package config

import (
	"strings"
	"testing"
)

func TestImageUpdatesPolicyValidation(t *testing.T) {
	host := "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {}\n---\n"
	for policy, want := range map[string]string{
		`{strategy: semver, constraint: "~1.27", allowTags: '^\d+\.\d+\.\d+$', ignoreTags: ["*-rc*"]}`: "",
		`{strategy: newest-build, allowTags: '^main-'}`:                                                "",
		`{enabled: true}`:                        "",
		`{strategy: latest}`:                     `strategy "latest" must be digest, semver or newest-build`,
		`{strategy: semver, constraint: "~>>1"}`: `constraint "~>>1"`,
		`{strategy: semver, allowTags: "("}`:     `allowTags "("`,
		`{strategy: semver, ignoreTags: ["["]}`:  `ignoreTags "["`,
		`{strategy: newest-build}`:               "newest-build requires allowTags",
		`{constraint: "~1"}`:                     "constraint only applies to the semver strategy",
		`{strategy: digest, allowTags: "."}`:     "only apply to the semver and newest-build",
	} {
		doc := host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: app}\nspec:\n  source: {path: charts/app}\n" +
			"  destination: {host: nas}\n  syncPolicy: {imageUpdates: " + policy + "}\n"
		_, err := Parse("apps.yaml", []byte(doc))
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", policy, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), "syncPolicy.imageUpdates: ") || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", policy, err, want)
		}
	}
}
