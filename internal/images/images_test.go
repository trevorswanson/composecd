package images

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const dig = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParse(t *testing.T) {
	for in, want := range map[string]Ref{
		"nginx":                        {Registry: "docker.io", Repository: "library/nginx", Tag: "latest"},
		"nginx:1.27":                   {Registry: "docker.io", Repository: "library/nginx", Tag: "1.27"},
		"grafana/grafana:11":           {Registry: "docker.io", Repository: "grafana/grafana", Tag: "11"},
		"ghcr.io/me/composecd:dev":     {Registry: "ghcr.io", Repository: "me/composecd", Tag: "dev"},
		"localhost:5000/app":           {Registry: "localhost:5000", Repository: "app", Tag: "latest"},
		"reg.lan:5000/a/b:v1@" + dig:   {Registry: "reg.lan:5000", Repository: "a/b", Tag: "v1", Digest: dig},
		"nginx@" + dig:                 {Registry: "docker.io", Repository: "library/nginx", Digest: dig},
		"docker.io/library/nginx:1.27": {Registry: "docker.io", Repository: "library/nginx", Tag: "1.27"},
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if a, b := mustParse(t, "nginx"), mustParse(t, "docker.io/library/nginx:latest"); a.Name() != b.Name() {
		t.Errorf("names differ: %s, %s", a.Name(), b.Name())
	}
	for _, bad := range []string{"", "nginx@sha256:short", "nginx:", "my image"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): expected an error", bad)
		}
	}
}

func mustParse(t *testing.T, s string) Ref {
	t.Helper()
	r, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const manifest = `name: web
services:
  web:
    image: nginx:1
  pinned:
    image: redis:7@` + dig + `
  off:
    image: nginx:1
    labels:
      composecd.image-updates: "false"
  on:
    image: busybox
    labels: ["composecd.image-updates=true"]
  built:
    image: me/app
    build: .
  templated:
    image: app:${TAG}
  noimage:
    build: .
`

func TestServices(t *testing.T) {
	got := map[string]Service{}
	svcs, err := Services(manifest, true, "composecd.image-updates")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range svcs {
		got[s.Name] = s
	}
	if len(got) != 6 {
		t.Fatalf("services = %+v", svcs)
	}
	if s := got["web"]; !s.Eligible() || !s.Checked() || s.Ref.Name() != "docker.io/library/nginx:1" {
		t.Errorf("web = %+v", s)
	}
	if s := got["pinned"]; s.Eligible() || !s.Checked() || !s.Pinned {
		t.Errorf("pinned = %+v", s)
	}
	if s := got["off"]; s.Eligible() || s.Checked() {
		t.Errorf("off = %+v", s)
	}
	if s := got["built"]; s.Eligible() || s.Skip == "" {
		t.Errorf("built = %+v", s)
	}
	if s := got["templated"]; s.Eligible() || s.Skip == "" {
		t.Errorf("templated = %+v", s)
	}

	// Off for the app: only the label opts in.
	svcs, _ = Services(manifest, false, "composecd.image-updates")
	for _, s := range svcs {
		if s.Eligible() != (s.Name == "on") {
			t.Errorf("app off: %s eligible = %v", s.Name, s.Eligible())
		}
	}
}

func TestPinManifest(t *testing.T) {
	pins := map[string]Pin{
		"web":    {Image: "nginx:1", Digest: dig},
		"pinned": {Image: "redis:7@" + dig, Digest: dig}, // never pinned twice
		"off":    {Image: "nginx:2", Digest: dig},        // not the image the service has
	}
	out, err := PinManifest(manifest, pins)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image: nginx:1@"+dig+"\n") {
		t.Errorf("web not pinned:\n%s", out)
	}
	if strings.Count(out, "@") != 2 {
		t.Errorf("want exactly two digests:\n%s", out)
	}
	again, err := PinManifest(out, pins)
	if err != nil || again != out {
		t.Errorf("pinning twice changed the manifest: %v\n%s", err, again)
	}
	if same, _ := PinManifest(manifest, nil); same != manifest {
		t.Error("no pins changed the manifest")
	}
}

func TestRegistryDigest(t *testing.T) {
	var heads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/me/app/manifests/dev" && r.Method == http.MethodHead:
			heads++
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				t.Errorf("Accept = %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", dig)
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/me/app/manifests/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	got, err := (&Registry{}).Digest(context.Background(), mustParse(t, host+"/me/app:dev"))
	if err != nil || got != dig || heads != 1 {
		t.Errorf("Digest = %q, %v (heads %d)", got, err, heads)
	}
	if _, err := (&Registry{}).Digest(context.Background(), mustParse(t, host+"/me/app:missing")); err == nil {
		t.Error("expected an error for a missing tag")
	}
}
