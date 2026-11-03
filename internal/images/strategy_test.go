package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPolicyValidate(t *testing.T) {
	for _, p := range []Policy{
		{},
		{Strategy: StrategyDigest},
		{Strategy: StrategySemver},
		{Strategy: StrategySemver, Constraint: "~1.27", AllowTags: `^\d+\.\d+\.\d+$`, IgnoreTags: []string{"*-rc*"}},
		{Strategy: StrategyNewestBuild, AllowTags: "^main-"},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for want, p := range map[string]Policy{
		"must be digest":           {Strategy: "newest"},
		"constraint \"~>1\"":       {Strategy: StrategySemver, Constraint: "~>1 ||| x"},
		"only applies to the semv": {Strategy: StrategyNewestBuild, Constraint: "1.x", AllowTags: "."},
		"allowTags \"(\"":          {Strategy: StrategySemver, AllowTags: "("},
		"requires allowTags":       {Strategy: StrategyNewestBuild},
		"ignoreTags \"[\"":         {Strategy: StrategySemver, IgnoreTags: []string{"["}},
		"only apply to the semver": {AllowTags: "."},
	} {
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), strings.Split(want, " \"")[0]) {
			t.Errorf("%+v: %v, want %q", p, err, want)
		}
	}
}

func TestPickSemver(t *testing.T) {
	tags := []string{"latest", "1.26.9", "1.27", "1.27.1", "1.27.3", "1.27.4-rc1", "1.27.3-alpine", "1.28.0", "1.28.1-rc.2", "2.0.0", "v1.29.0", "mainline"}
	for _, c := range []struct {
		name    string
		p       Policy
		current string
		want    string
	}{
		{"default: same major", Policy{Strategy: StrategySemver}, "1.27.1", "v1.29.0"},
		{"tilde", Policy{Strategy: StrategySemver, Constraint: "~1.27"}, "1.27.1", "1.27.3"},
		{"major 2: 2.0.0 is no newer than 2", Policy{Strategy: StrategySemver}, "2", "2"},
		{"allowTags", Policy{Strategy: StrategySemver, AllowTags: `^\d+\.\d+\.\d+$`}, "1.27.1", "1.28.0"},
		{"ignoreTags", Policy{Strategy: StrategySemver, Constraint: "<2", IgnoreTags: []string{"v*", "1.28.*"}}, "1.27.1", "1.27.3"},
		{"prerelease named", Policy{Strategy: StrategySemver, Constraint: ">=1.28.0-0 <1.29.0"}, "1.27.1", "1.28.1-rc.2"},
		{"never older than current", Policy{Strategy: StrategySemver, Constraint: "~1.26"}, "1.27.1", "1.27.1"},
		{"nothing newer", Policy{Strategy: StrategySemver, Constraint: "~1.27"}, "1.27.3", "1.27.3"},
	} {
		got, err := c.p.PickSemver(c.current, tags)
		if err != nil || got != c.want {
			t.Errorf("%s: PickSemver(%s) = %q, %v; want %q", c.name, c.current, got, err, c.want)
		}
	}
	if _, err := (Policy{Strategy: StrategySemver}).PickSemver("latest", tags); err == nil {
		t.Error("a tag that isn't semver: expected an error")
	}
	if c, _ := (Policy{Strategy: StrategySemver}).ConstraintFor("0.4.2"); c != "0.x" {
		t.Errorf("default constraint = %q", c)
	}
}

func TestPolicyAccepts(t *testing.T) {
	semverP := Policy{Strategy: StrategySemver, Constraint: "~1.27", IgnoreTags: []string{"*.9"}}
	newest := Policy{Strategy: StrategyNewestBuild, AllowTags: "^main-"}
	for _, c := range []struct {
		p            Policy
		current, tag string
		want         bool
	}{
		{semverP, "1.27.1", "1.27.3", true},
		{semverP, "1.27.3", "1.27.1", false}, // older
		{semverP, "1.27.1", "1.28.0", false}, // outside the constraint
		{semverP, "1.27.1", "1.27.9", false}, // ignored
		{semverP, "1.27.1", "latest", false},
		{newest, "main-1", "main-0", true},
		{newest, "main-1", "dev", false},
		{Policy{}, "1", "2", false},
	} {
		if got := c.p.Accepts(c.current, c.tag); got != c.want {
			t.Errorf("%+v.Accepts(%s, %s) = %v", c.p, c.current, c.tag, got)
		}
	}
}

func TestNewestCandidates(t *testing.T) {
	defer func(n int) { NewestBuildCandidates = n }(NewestBuildCandidates)
	NewestBuildCandidates = 3
	p := Policy{Strategy: StrategyNewestBuild, AllowTags: "^build-", IgnoreTags: []string{"*-debug"}}
	tags := []string{"latest", "build-9", "build-10", "build-11-debug", "build-2", "build-100", "build-1"}
	got := p.NewestCandidates("build-1", tags)
	if want := []string{"build-100", "build-10", "build-9", "build-1"}; !slices.Equal(got, want) {
		t.Errorf("candidates = %q, want %q", got, want)
	}
}

func TestServicesPolicyLabels(t *testing.T) {
	const m = `services:
  app:
    image: nginx:1.27.1
  label:
    image: nginx:1.27.1
    labels:
      composecd.image-updates.constraint: "~1.27"
  newest:
    image: me/app:main-1
    labels: ["composecd.image-updates.strategy=newest-build", "composecd.image-updates.allow-tags=^main-"]
  bad:
    image: nginx:1.27.1
    labels:
      composecd.image-updates.strategy: newest-build
  latest:
    image: nginx
  git:
    image: nginx:1.27.1@` + dig + `
`
	app := Policy{Strategy: StrategySemver, Constraint: "1.x", IgnoreTags: []string{"*-rc*"}}
	svcs, err := Services(m, true, app, "composecd.image-updates")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Service{}
	for _, s := range svcs {
		got[s.Name] = s
	}
	if s := got["app"]; !s.MovesTags() || s.Policy.Constraint != "1.x" || s.Warning != "" {
		t.Errorf("app = %+v", s)
	}
	if s := got["label"]; !s.MovesTags() || s.Policy.Constraint != "~1.27" || len(s.Policy.IgnoreTags) != 1 {
		t.Errorf("label = %+v", s)
	}
	if s := got["newest"]; !s.MovesTags() || s.Policy.Strategy != StrategyNewestBuild || s.Policy.AllowTags != "^main-" || s.Policy.Constraint != "" {
		t.Errorf("newest = %+v", s)
	}
	if s := got["bad"]; s.MovesTags() || s.Policy.Name() != StrategyDigest || !strings.Contains(s.Warning, "requires allowTags") || !s.Eligible() {
		t.Errorf("bad = %+v", s)
	}
	if s := got["latest"]; s.MovesTags() || !s.Eligible() || !strings.Contains(s.Warning, "isn't a semantic version") {
		t.Errorf("latest = %+v", s)
	}
	if s := got["git"]; s.MovesTags() || !s.Pinned {
		t.Errorf("git = %+v", s)
	}
}

func TestOverrideMovesTags(t *testing.T) {
	const m = "services:\n  web:\n    image: ghcr.io/me/web:1.27.1\n  git:\n    image: nginx:1@" + dig + "\n"
	out, err := Override(m, map[string]Pin{
		"web": {Image: "ghcr.io/me/web:1.27.1", Tag: "1.27.3", Digest: dig},
		"git": {Image: "nginx:1@" + dig, Tag: "2", Digest: dig},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image: ghcr.io/me/web:1.27.3@"+dig+"\n") || !strings.Contains(out, "image: nginx:1@"+dig+"\n") {
		t.Errorf("override:\n%s", out)
	}
	for in, want := range map[string]string{"nginx": "nginx", "nginx:1": "nginx", "localhost:5000/a/b:v1@" + dig: "localhost:5000/a/b", "localhost:5000/a": "localhost:5000/a"} {
		if got := Repo(in); got != want {
			t.Errorf("Repo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistryTags(t *testing.T) {
	pages := [][]string{{"1.0", "1.1"}, {"1.2", "2.0"}, {"3.0"}}
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		if r.URL.Path != "/v2/me/app/tags/list" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page+1 < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`</v2/me/app/tags/list?page=%d>; rel="next"`, page+1))
		}
		fmt.Fprintf(w, `{"name":"me/app","tags":["%s"]}`, strings.Join(pages[page], `","`))
	}))
	defer srv.Close()
	ref := mustParse(t, strings.TrimPrefix(srv.URL, "http://")+"/me/app:1.0")

	tags, truncated, err := (&Registry{}).Tags(context.Background(), ref)
	if err != nil || truncated || len(tags) != 5 || requests != 3 {
		t.Errorf("Tags = %q, %v, %v (%d requests)", tags, truncated, err, requests)
	}
	tags, truncated, err = (&Registry{MaxTags: 3}).Tags(context.Background(), ref)
	if err != nil || !truncated || !slices.Equal(tags, []string{"1.0", "1.1", "1.2"}) {
		t.Errorf("capped Tags = %q, %v, %v", tags, truncated, err)
	}
}

func TestRegistryCreated(t *testing.T) {
	sum := func(b string) string {
		h := sha256.Sum256([]byte(b))
		return "sha256:" + hex.EncodeToString(h[:])
	}
	config := `{"created":"2026-10-30T12:00:00Z","architecture":"amd64","os":"linux"}`
	image := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		sum(config), len(config))
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"os":"unknown","architecture":"unknown"}},`+
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"os":"linux","architecture":"amd64"}}]}`,
		sum("attestation"), len("attestation"), sum(image), len(image))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve := func(mediaType, body string) {
			w.Header().Set("Content-Type", mediaType)
			w.Header().Set("Docker-Content-Digest", sum(body))
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			fmt.Fprint(w, body)
		}
		switch r.URL.Path {
		case "/v2/":
		case "/v2/me/app/manifests/" + sum(index):
			serve("application/vnd.oci.image.index.v1+json", index)
		case "/v2/me/app/manifests/" + sum(image):
			serve("application/vnd.oci.image.manifest.v1+json", image)
		case "/v2/me/app/blobs/" + sum(config):
			serve("application/octet-stream", config)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ref := mustParse(t, strings.TrimPrefix(srv.URL, "http://")+"/me/app:main-1")
	ref.Digest = sum(index)
	got, err := (&Registry{}).Created(context.Background(), ref)
	if err != nil || !got.Equal(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("Created = %v, %v", got, err)
	}
}
