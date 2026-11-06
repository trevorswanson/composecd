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

func TestImagePolicyOffer(t *testing.T) {
	no := false
	def := &ImageOffer{Constraint: ">=1", IgnoreTags: []string{"*-rc*"}}
	app := func(u *AppImageUpdates) *Application {
		a := &Application{}
		a.Spec.SyncPolicy.ImageUpdates = u
		return a
	}
	if p := ImagePolicy(nil, app(nil)); p.Offer != nil {
		t.Errorf("no offer anywhere: %+v", p.Offer)
	}
	// The default applies to an app that sets nothing.
	if p := ImagePolicy(def, app(nil)); p.Offer == nil || p.Offer.Constraint != ">=1" || len(p.Offer.IgnoreTags) != 1 {
		t.Errorf("default: %+v", p.Offer)
	}
	// An app's fields replace the default's, one by one.
	p := ImagePolicy(def, app(&AppImageUpdates{Strategy: "semver", Offer: &ImageOffer{Constraint: ">=2"}}))
	if p.Strategy != "semver" || p.Offer == nil || p.Offer.Constraint != ">=2" || len(p.Offer.IgnoreTags) != 1 {
		t.Errorf("merged: %+v %+v", p, p.Offer)
	}
	// enabled: false turns the default off for one app, or the default itself.
	if p := ImagePolicy(def, app(&AppImageUpdates{Offer: &ImageOffer{Enabled: &no}})); p.Offer != nil {
		t.Errorf("turned off: %+v", p.Offer)
	}
	if p := ImagePolicy(&ImageOffer{Enabled: &no}, app(nil)); p.Offer != nil {
		t.Errorf("default off: %+v", p.Offer)
	}
	if p := ImagePolicy(nil, app(&AppImageUpdates{Offer: &ImageOffer{}})); p.Offer == nil || p.Offer.Constraint != "*" {
		t.Errorf("app only: %+v", p.Offer)
	}
}

func TestBootstrapOffer(t *testing.T) {
	b, err := ParseBootstrap([]byte("root: {repoURL: x}\nimageUpdates: {offer: {constraint: \">=1\"}}\n"))
	if err != nil || b.ImageUpdates.Offer.Policy() == nil || b.ImageUpdates.Offer.Policy().Constraint != ">=1" {
		t.Fatalf("offer: %+v, %v", b, err)
	}
	for doc, want := range map[string]string{
		"imageUpdates: {offer: {strategy: newest-build}}": `imageUpdates.offer: strategy "newest-build" must be semver`,
		"imageUpdates: {offer: {constraint: \"~>>1\"}}":   `imageUpdates.offer: constraint "~>>1"`,
	} {
		if _, err := ParseBootstrap([]byte("root: {repoURL: x}\n" + doc + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}
}
