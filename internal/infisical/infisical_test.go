package infisical

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

func write(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUniversalAuthSecrets(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			logins.Add(1)
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["clientId"] != "id" || in["clientSecret"] != "shh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"accessToken":"tok","expiresIn":7200}`))
		case "/api/v3/secrets/raw":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			q := r.URL.Query()
			if q.Get("workspaceId") != "proj" || q.Get("environment") != "prod" || q.Get("secretPath") != "/media" || q.Get("recursive") != "true" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"secrets":[{"secretKey":"DB","secretValue":"own"}],"imports":[{"secrets":[{"secretKey":"DB","secretValue":"imported"},{"secretKey":"X","secretValue":"x"}]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(&config.Infisical{SiteURL: srv.URL, ClientID: "id", ClientSecretFile: write(t, "shh\n")})
	src := &config.InfisicalSource{ProjectID: "proj", Environment: "prod", Path: "/media", Recursive: true}
	for i := 0; i < 2; i++ {
		got, err := c.Secrets(context.Background(), src)
		if err != nil {
			t.Fatal(err)
		}
		// Imports first so the folder's own secret wins when merged in order.
		if len(got) != 3 || got[0].Value != "imported" || got[2] != (Secret{"DB", "own"}) {
			t.Fatalf("secrets = %#v", got)
		}
	}
	if logins.Load() != 1 {
		t.Fatalf("logins = %d, want the session reused", logins.Load())
	}
}

func TestTokenFileAndRetryOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"secrets":[{"secretKey":"K","secretValue":"v"}]}`))
	}))
	defer srv.Close()
	f := write(t, "good\n")
	c := New(&config.Infisical{SiteURL: srv.URL, TokenFile: f})
	got, err := c.Secrets(context.Background(), &config.InfisicalSource{ProjectID: "p", Environment: "e"})
	if err != nil || len(got) != 1 || got[0].Value != "v" {
		t.Fatalf("%#v %v", got, err)
	}
	if err := os.WriteFile(f, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Secrets(context.Background(), &config.InfisicalSource{ProjectID: "p", Environment: "e"}); err == nil {
		t.Fatal("expected an error for a rejected token")
	}
}

func TestErrorsHideBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("private-secret"))
	}))
	defer srv.Close()
	c := New(&config.Infisical{SiteURL: srv.URL, TokenFile: write(t, "t")})
	_, err := c.Secrets(context.Background(), &config.InfisicalSource{ProjectID: "private-project", Environment: "e"})
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("err = %v", err)
	}
	c = New(&config.Infisical{SiteURL: "http://127.0.0.1:1", TokenFile: write(t, "t")})
	if _, err = c.Secrets(context.Background(), &config.InfisicalSource{ProjectID: "private-project", Environment: "e"}); err == nil || strings.Contains(err.Error(), "private-project") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewNil(t *testing.T) {
	if New(nil) != nil {
		t.Fatal("expected nil")
	}
}
