package source

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func app(src config.AppSource) *config.Application {
	return &config.Application{Metadata: config.Metadata{Name: "media"}, Spec: config.AppSpec{Source: src}}
}

func TestPrepareGitPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"stacks/media/Chart.yaml":      "name: media\nversion: 0.1.0\n",
		"stacks/media/values-nas.yaml": "apps: {sonarr: {enabled: true, port: 8989}}\ntag: '1'\n",
		"shared/values.yaml":           "domain: home.lan\ntag: '2'\n",
	})
	host := &config.Host{Spec: config.HostSpec{Env: map[string]string{"TZ": "UTC", "PUID": "0"}}}
	a := app(config.AppSource{
		Path:       "stacks/media",
		ValueFiles: []string{"values-nas.yaml", "../../shared/values.yaml"},
		Values:     map[string]interface{}{"apps": map[string]interface{}{"sonarr": map[string]interface{}{"port": 9000}}},
		Env:        map[string]string{"PUID": "1000"},
	})
	p, err := Prepare(context.Background(), a, host, root, "0123456789abcdef", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	if p.ChartRef != filepath.Join(realRoot, "stacks", "media") || p.Revision != "0123456789abcdef" {
		t.Errorf("chart %q revision %q", p.ChartRef, p.Revision)
	}
	want := map[string]interface{}{
		"apps":   map[string]interface{}{"sonarr": map[string]interface{}{"enabled": true, "port": float64(9000)}},
		"domain": "home.lan",
		"tag":    "2",
	}
	if !reflect.DeepEqual(p.Values, want) {
		t.Errorf("values = %#v", p.Values)
	}
	if !reflect.DeepEqual(p.Env, []string{"PUID=1000", "TZ=UTC"}) {
		t.Errorf("env = %v", p.Env)
	}
	if v, ok := p.Lookup("TZ"); !ok || v != "UTC" {
		t.Errorf("lookup TZ = %q %v", v, ok)
	}
	if _, ok := p.Lookup("HOME"); ok {
		t.Error("lookup should not see the process environment")
	}
}

func TestPrepareErrors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, root, map[string]string{"chart/Chart.yaml": "name: c\n", "chart/bad.yaml": "a: [\n", "notachart/x": ""})
	write(t, outside, map[string]string{"secret.yaml": "pw: x\n"})
	cases := map[string]config.AppSource{
		"missing path":   {Path: "nope"},
		"not a chart":    {Path: "notachart"},
		"missing values": {Path: "chart", ValueFiles: []string{"missing.yaml"}},
		"bad values":     {Path: "chart", ValueFiles: []string{"bad.yaml"}},
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(root, "chart", "link.yaml")); err != nil {
			t.Fatal(err)
		}
		cases["symlink escape"] = config.AppSource{Path: "chart", ValueFiles: []string{"link.yaml"}}
	}
	for name, src := range cases {
		_, err := Prepare(context.Background(), app(src), &config.Host{}, root, "abc", nil, "")
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if strings.Contains(err.Error(), root) {
			t.Errorf("%s: error leaks the checkout path: %v", name, err)
		}
	}
}

func TestPrepareOCI(t *testing.T) {
	a := app(config.AppSource{Chart: "oci://ghcr.io/me/charts/media", Version: "^1", Values: map[string]interface{}{"x": 1}})
	p, err := Prepare(context.Background(), a, &config.Host{}, "", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChartRef != "oci://ghcr.io/me/charts/media" || p.Version != "^1" || p.Values["x"] != float64(1) || p.Revision != "" {
		t.Errorf("prepared = %+v", p)
	}
}
