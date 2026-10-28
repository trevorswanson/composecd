package source

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

func TestPrepareOCIGitValues(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"values/base.yaml": "tag: first\nnested: {base: true}\n", "values/override.yaml": "tag: second\nnested: {extra: true}\n"})
	a := app(config.AppSource{Chart: "oci://registry.lan/charts/app", Version: "^1", ValuesFrom: &config.GitSource{}, ValueFiles: []string{"values/base.yaml", "values/override.yaml"}, Values: map[string]interface{}{"tag": "inline"}})
	p, err := Prepare(context.Background(), a, &config.Host{}, root, "values-sha", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Values["tag"] != "inline" || p.ValuesRevision != "values-sha" || p.Revision != "" || p.ChartRef != a.Spec.Source.Chart || p.Version != "^1" {
		t.Fatalf("%+v", p)
	}
	nested := p.Values["nested"].(map[string]interface{})
	if nested["base"] != true || nested["extra"] != true {
		t.Fatal(nested)
	}
	a.Spec.Source.Values = nil
	p, err = Prepare(context.Background(), a, &config.Host{}, root, "values-sha", nil, "")
	if err != nil || p.Values["tag"] != "second" {
		t.Fatalf("file precedence: %+v %v", p, err)
	}
}
func TestOCIValuesCannotEscapeRepository(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write(t, outside, map[string]string{"secret.yaml": "password: private-secret\n"})
	paths := []string{"../outside.yaml", "missing.yaml"}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(root, "escape.yaml")); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, "escape.yaml")
	}
	for _, path := range paths {
		a := app(config.AppSource{Chart: "oci://registry.lan/charts/app", ValuesFrom: &config.GitSource{}, ValueFiles: []string{path}})
		if _, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, ""); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("%s: %v", path, err)
		}
	}
	a := app(config.AppSource{Chart: "oci://registry.lan/charts/app", ValueFiles: []string{"file.yaml"}})
	if _, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, ""); err == nil {
		t.Fatal("accepted OCI files without valuesFrom")
	}
}
func TestOCIValuesUseSOPS(t *testing.T) {
	key := fakeSOPS(t)
	root := t.TempDir()
	write(t, root, map[string]string{"secret.sops.yaml": "sops: {age: []}\npassword: ENC[data]\n"})
	a := app(config.AppSource{Chart: "oci://registry.lan/charts/app", ValuesFrom: &config.GitSource{}, ValueFiles: []string{"secret.sops.yaml"}})
	p, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, key)
	if err != nil || p.Values["password"] != "private-secret" || len(p.Secrets) == 0 || p.ValuesRevision != "abc" {
		t.Fatalf("%+v %v", p, err)
	}
	objs := &config.Objects{Applications: []*config.Application{a}}
	write(t, root, map[string]string{"secret.sops.yaml": "password: accidentally-plain\n"})
	if err := ValidateSecrets(root, objs); err == nil || !strings.Contains(err.Error(), "no SOPS metadata") {
		t.Fatal(err)
	}
}
