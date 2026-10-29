package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/infisical"
)

func fakeInfisical(t *testing.T, body string, status int) *infisical.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	return infisical.New(&config.Infisical{SiteURL: srv.URL, TokenFile: tok})
}

func TestPrepareInfisical(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"chart/Chart.yaml": "name: c\n", "chart/values.yaml": "db: {host: plain, password: old}\ntag: file\n"})
	c := fakeInfisical(t, `{"secrets":[{"secretKey":"password","secretValue":"s3cret-value"},{"secretKey":"short","secretValue":"ab"}]}`, 200)
	inf := &config.InfisicalSource{ProjectID: "p", Environment: "e", Into: "db"}
	a := app(config.AppSource{Path: "chart", ValueFiles: []string{"values.yaml"}, Infisical: inf, Values: map[string]interface{}{"tag": "inline"}})
	p, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, "", c)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{"host": "plain", "password": "s3cret-value", "short": "ab"}
	if !reflect.DeepEqual(p.Values["db"], want) || p.Values["tag"] != "inline" {
		t.Fatalf("values = %#v", p.Values)
	}
	if !reflect.DeepEqual(p.SecretPaths, [][]string{{"db", "password"}, {"db", "short"}}) {
		t.Fatalf("paths = %#v", p.SecretPaths)
	}
	found := false
	for _, s := range p.Secrets {
		found = found || s == "s3cret-value"
		if s == "ab" {
			t.Fatalf("secrets = %#v: short values aren't maskable", p.Secrets)
		}
	}
	if !found {
		t.Fatalf("secrets = %#v", p.Secrets)
	}
}

func TestPrepareInfisicalErrors(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"chart/Chart.yaml": "name: c\n"})
	a := app(config.AppSource{Path: "chart", Infisical: &config.InfisicalSource{ProjectID: "p", Environment: "e"}})
	if _, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, ""); err == nil || !strings.Contains(err.Error(), "infisical section") {
		t.Fatalf("unconfigured: %v", err)
	}
	c := fakeInfisical(t, `private-secret`, 500)
	if _, err := Prepare(context.Background(), a, &config.Host{}, root, "abc", nil, "", c); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("server error: %v", err)
	}
}
