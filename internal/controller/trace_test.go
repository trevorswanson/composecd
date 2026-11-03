package controller

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trevorswanson/composecd/internal/images"
)

// writeChart writes a chart with templates/a.yaml and values.yaml.
func writeChart(t *testing.T, appVersion, template, values string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"Chart.yaml":         "apiVersion: v2\nname: web\nversion: 0.1.0\nappVersion: '" + appVersion + "'\n",
		"values.yaml":        values,
		"templates/a.yaml":   template,
		"templates/_x.tpl":   `{{ define "web.image" }}{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}{{ end }}`,
		"templates/notes.md": "",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTrace(t *testing.T) {
	for name, c := range map[string]struct {
		template, values, appVersion, label string
		user                                map[string]interface{}
		// keys are the sources found, as key:feeds.
		keys []string
		// want is the value the first key needs for nginx:9.9.9 on web.
		want string
	}{
		"direct": {
			template: "services:\n  web:\n    image: nginx:{{ .Values.tag }}\n",
			values:   "tag: 1.27.1\nreplicas: 1\n",
			keys:     []string{"tag:web"}, want: "9.9.9",
		},
		"repository and tag in two keys": {
			template: "services:\n  web:\n    image: {{ .Values.image.repository }}:{{ .Values.image.tag }}\n",
			values:   "image: {repository: nginx, tag: 1.27.1}\n",
			keys:     []string{"image.tag:web"}, want: "9.9.9",
		},
		"printf in a define": {
			template: "services:\n  web:\n    image: {{ include \"web.image\" . }}\n",
			values:   "image: {repository: nginx, tag: 1.27.1}\n",
			keys:     []string{"image.tag:web"}, want: "9.9.9",
		},
		"a prefix in the template": {
			template: "services:\n  web:\n    image: nginx:v{{ .Values.version }}\n",
			values:   "version: 1.27.1\n",
			keys:     []string{"version:web"}, want: "",
		},
		"Chart.AppVersion": {
			template:   "services:\n  web:\n    image: nginx:{{ .Chart.AppVersion }}\n",
			values:     "replicas: 1\n",
			appVersion: "1.27.1",
			keys:       []string{KeyAppVersion + ":web"}, want: "9.9.9",
		},
		"a literal in the template": {
			template: "services:\n  web:\n    image: nginx:1.27.1\n",
			values:   "tag: 1.27.1\n",
		},
		"a global tag feeds two services": {
			template: "services:\n  web:\n    image: nginx:{{ .Values.global.tag }}\n  worker:\n    image: nginx:{{ .Values.global.tag }}\n",
			values:   "global: {tag: 1.27.1}\n",
			keys:     []string{"global.tag:web,worker"}, want: "9.9.9",
		},
		"a user value wins": {
			template: "services:\n  web:\n    image: nginx:{{ .Values.tag }}\n",
			values:   "tag: '1.0'\n",
			user:     map[string]interface{}{"tag": "1.27.1"},
			keys:     []string{"tag:web"}, want: "9.9.9",
		},
		"two keys that could be it": {
			template: "services:\n  web:\n    image: nginx:{{ .Values.tag | default .Values.fallback }}\n",
			values:   "tag: 1.27.1\nfallback: 1.27.1\nother: 1.27.1\n",
			keys:     []string{"tag:web"}, want: "9.9.9",
		},
		"the label names the key": {
			template: "services:\n  web:\n    image: nginx:{{ coalesce .Values.a .Values.b }}\n",
			values:   "a: 1.27.1\nb: 1.27.1\n",
			label:    "a",
			keys:     []string{"a:web"}, want: "9.9.9",
		},
		"the label names a key that isn't it": {
			template: "services:\n  web:\n    image: nginx:1.27.1\n",
			values:   "a: 1.27.1\n",
			label:    "a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeChart(t, c.appVersion, c.template, c.values)
			user := c.user
			if user == nil {
				user = map[string]interface{}{}
			}
			tr, err := newTracer(context.Background(), "web", dir, "", nil, func(string) (string, bool) { return "", false }, user)
			if err != nil {
				t.Fatal(err)
			}
			base, err := tr.images(user, "")
			if err != nil {
				t.Fatal(err)
			}
			srcs, err := tr.trace("web", base, c.label)
			if err != nil {
				t.Fatal(err)
			}
			want := images.WithTag(base["web"], "9.9.9")
			if c.want == "" {
				want = "nginx:v9.9.9"
			}
			srcs = forImage(srcs, want)
			var keys []string
			for _, s := range srcs {
				k := s.Key + ":"
				for i, f := range s.feeds {
					if i > 0 {
						k += ","
					}
					k += f
				}
				keys = append(keys, k)
			}
			if !slices.Equal(keys, c.keys) {
				t.Fatalf("sources = %q, want %q", keys, c.keys)
			}
			if len(srcs) == 0 {
				return
			}
			got, ok := srcs[0].valueFor(want)
			if c.want == "" {
				// nginx:v{{ .Values.version }}: the key holds 9.9.9 for v9.9.9.
				c.want = "9.9.9"
			}
			if !ok || got != c.want {
				t.Errorf("value for %s = %q, %v, want %q", want, got, ok, c.want)
			}
		})
	}
}
