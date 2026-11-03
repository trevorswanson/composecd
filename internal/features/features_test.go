package features

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
)

// fields lists the YAML paths of t's settings: the leaves, and the sections
// and lists above them, with [] for list items. A type that marshals itself
// (a duration) is a leaf. Paths in stop aren't descended into.
func fields(t reflect.Type, prefix string, stop map[string]bool, out map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	marshaler := reflect.TypeOf((*yaml.Marshaler)(nil)).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" || name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		out[path] = true
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if stop[path] || ft.Implements(marshaler) || reflect.PointerTo(ft).Implements(marshaler) {
			continue
		}
		switch {
		case ft.Kind() == reflect.Struct:
			fields(ft, path, stop, out)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			fields(ft.Elem(), path+"[]", stop, out)
		}
	}
}

// TestRegistryCoversConfig fails when a setting has no feature, or a feature
// names no setting: add a new field to registry.go.
func TestRegistryCoversConfig(t *testing.T) {
	for scope, typ := range map[Scope]interface{}{
		Server:         config.Bootstrap{},
		Host:           config.Host{},
		Application:    config.Application{},
		ApplicationSet: config.ApplicationSet{},
	} {
		paths := map[string]bool{}
		// The template is an Application, explained as one.
		fields(reflect.TypeOf(typ), "", map[string]bool{"spec.template": true}, paths)
		listed := map[string]bool{}
		for _, f := range For(scope) {
			if listed[f.Path] {
				t.Errorf("%s: %s is listed twice", scope, f.Path)
			}
			listed[f.Path] = true
			if !paths[f.Path] {
				t.Errorf("%s: %s is not a setting", scope, f.Path)
			}
			if !f.Item() && (f.Group == "" || f.Title == "") {
				t.Errorf("%s: %s needs a group and a title", scope, f.Path)
			}
		}
		for path := range paths {
			if listed[path] {
				continue
			}
			// Sections need no entry of their own when their fields have one.
			section := false
			for l := range listed {
				section = section || strings.HasPrefix(l, path+".") || strings.HasPrefix(l, path+"[]")
			}
			if !section && path != "metadata.name" && path != "spec" && path != "spec.template" {
				t.Errorf("%s: %s has no feature in registry.go", scope, path)
			}
		}
	}
}

func setting(t *testing.T, groups []Group, path string) Setting {
	t.Helper()
	for _, g := range groups {
		for _, s := range g.Settings {
			if s.Path == path {
				return s
			}
		}
	}
	t.Fatalf("no setting %s", path)
	return Setting{}
}

func TestExplainApplication(t *testing.T) {
	no := false
	app := &config.Application{Spec: config.AppSpec{
		// The loader fills in the root's repository and revision.
		Source:      config.AppSource{RepoURL: "https://git.lan/home.git", TargetRevision: "main", Path: "stacks/media", ValueFiles: []string{"a.yaml", "b.yaml"}, Values: map[string]interface{}{"tag": "secret-value"}},
		Destination: config.Destination{Host: "nas"},
		SyncPolicy: config.SyncPolicy{
			Automated: &config.Automated{SelfHeal: true},
			Options:   config.SyncOptions{Wait: &no, Timeout: config.Duration(10 * time.Minute)},
		},
	}}
	groups, err := Explain(Application, app, Context{
		Boot: &config.Bootstrap{Root: config.Root{RepoURL: "https://git.lan/home.git", TargetRevision: "main"},
			ImageUpdates: config.ImageUpdates{Enabled: true}},
		Prefix: "spec.template",
		Mask:   func(s string) string { return strings.ReplaceAll(s, "secret-value", "••••") },
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]Setting{
		"spec.source.path":                      {State: Set, Value: "stacks/media"},
		"spec.source.repoURL":                   {State: Inherited, Value: "https://git.lan/home.git", From: "the root repository"},
		"spec.source.targetRevision":            {State: Inherited, Value: "main", From: "the root's revision"},
		"spec.source.valueFiles":                {State: Set, Value: "a.yaml, b.yaml"},
		"spec.source.values":                    {State: Set, Value: "tag: ••••", Block: true},
		"spec.syncPolicy.automated":             {State: Set, Value: "On"},
		"spec.syncPolicy.automated.selfHeal":    {State: Set, Value: "true"},
		"spec.syncPolicy.automated.prune":       {State: Default},
		"spec.syncPolicy.options.wait":          {State: Set, Value: "false"},
		"spec.syncPolicy.options.timeout":       {State: Set, Value: "10m0s"},
		"spec.syncPolicy.retry":                 {State: Default},
		"spec.syncPolicy.imageUpdates.enabled":  {State: Inherited, Value: "true", From: "imageUpdates.enabled in composecd.yaml"},
		"spec.syncPolicy.imageUpdates.strategy": {State: Default},
	} {
		got := setting(t, groups, path)
		if got.State != want.State || got.Value != want.Value || got.Block != want.Block || got.From != want.From {
			t.Errorf("%s: %+v, want %+v", path, got, want)
		}
	}
	if s := setting(t, groups, "spec.syncPolicy.retry.limit").Snippet; s != "spec:\n  template:\n    spec:\n      syncPolicy:\n        retry:\n          limit: 3" {
		t.Errorf("snippet:\n%s", s)
	}
	if groups[0].Name != "General" || groups[0].Count != 1 {
		t.Errorf("first group: %+v", groups[0])
	}
}

func TestExplainServerHidesSecretsAndDefaults(t *testing.T) {
	written := []byte(`root: {repoURL: "https://git.lan/me/home.git"}
repositories:
  - url: https://ci:hunter2@git.lan/
    passwordFile: /run/secrets/git
  - url: https://ghp_tokenvalue@github.com/
notifications:
  - {name: chat, type: discord, url: "https://discord.com/api/webhooks/1/token"}
`)
	b, err := config.ParseBootstrap(written)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := Explain(Server, b, Context{Written: written})
	if err != nil {
		t.Fatal(err)
	}
	if s := setting(t, groups, "listen"); s.State != Default || s.Default != ":8080" {
		t.Errorf("listen: %+v", s)
	}
	if s := setting(t, groups, "root.repoURL"); s.State != Set {
		t.Errorf("root: %+v", s)
	}
	repos := setting(t, groups, "repositories")
	if !repos.Block || strings.Contains(repos.Value, "hunter2") || strings.Contains(repos.Value, "ghp_tokenvalue") || !strings.Contains(repos.Value, "/run/secrets/git") || len(repos.Fields) != 6 {
		t.Errorf("repositories: %+v", repos)
	}
	if s := setting(t, groups, "notifications"); strings.Contains(s.Value, "token") || !strings.Contains(s.Value, "url: (hidden)") {
		t.Errorf("notifications: %s", s.Value)
	}
	if s := setting(t, groups, "webhook.secretFile"); s.State != Default || !strings.Contains(s.Snippet, "webhook:\n  secretFile:") {
		t.Errorf("webhook: %+v", s)
	}
}

func TestExplainLabels(t *testing.T) {
	g := ExplainLabels(map[string]map[string]string{
		"web": {config.LabelImageUpdates + ".strategy": "semver"},
		"db":  {config.LabelImageUpdates: "false", "other": "x"},
	})
	if g.Count != 2 || g.Settings[0].Value != "db: false" || g.Settings[1].Value != "web: semver" || g.Settings[2].State != Default {
		t.Errorf("%+v", g)
	}
}

func TestExplainShowsReadonlyUsersEnvKeysOnly(t *testing.T) {
	h := &config.Host{Spec: config.HostSpec{Env: map[string]string{"TZ": "UTC", "API_TOKEN": "env-secret"}}}
	for _, keysOnly := range []bool{false, true} {
		groups, err := Explain(Host, h, Context{KeysOnly: keysOnly})
		if err != nil {
			t.Fatal(err)
		}
		v := setting(t, groups, "spec.env").Value
		if strings.Contains(v, "env-secret") == keysOnly || !strings.Contains(v, "API_TOKEN") {
			t.Errorf("keysOnly %t: %s", keysOnly, v)
		}
	}
}
