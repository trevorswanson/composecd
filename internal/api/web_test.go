package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/store"
)

type webStub struct {
	stub
	updates chan controller.Update
}

func (s *webStub) App(name string) (controller.AppStatus, error) {
	a, err := s.stub.App(name)
	a.Health.Containers = []compose.Container{{Name: "media-web-1", Image: "nginx:1", State: "running", Health: "healthy"}}
	a.Sync.Changes = []controller.Change{{Resource: "services/web", Op: "changed", Diff: "-password: secret-value\n+password: <script>\n"}}
	return a, err
}
func (s *webStub) Subscribe() (<-chan controller.Update, func()) { return s.updates, func() {} }
func (s *webStub) Operations(string, int) ([]*store.Operation, error) {
	return []*store.Operation{{ID: 7}}, nil
}
func (s *webStub) Operation(int64) (*store.Operation, error) {
	return &store.Operation{ID: 7, App: "media", User: "operator", Log: "secret-value <script>"}, nil
}

func TestWebPagesAndActions(t *testing.T) {
	a := newAuthn(t, "secret")
	st := &webStub{updates: make(chan controller.Update, 1)}
	srv := httptest.NewServer(New(st, Options{Auth: a}))
	defer srv.Close()
	b := newBrowser(t, srv)
	b.login("", "secret")
	cases := map[string]string{
		"/": "Applications", "/apps?host=nas&sync=Synced&health=Healthy": "<strong>media</strong>",
		"/apps?label=missing": "No applications match", "/hosts": "Start maintenance", "/windows": "nightly",
		"/operations": "operator", "/settings": "Create token",
		"/apps/media": "Desired revision", "/apps/media?tab=containers": "media-web-1",
		"/apps/media?tab=diff": "services/web", "/apps/media?tab=history": "Rollback",
		"/apps/media?tab=operations": "Output",
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			code, _, body := b.do("GET", path, "", nil)
			if code != 200 || !strings.Contains(body, want) {
				t.Fatalf("%d: %s", code, body)
			}
			if strings.Contains(body, "secret-value") || strings.Contains(body, "<script>") {
				t.Fatal("unmasked or unescaped output")
			}
		})
	}
	_, _, body := b.do("GET", "/apps/media", "", nil)
	for _, want := range []string{"hx-headers=", "X-CSRF-Token", b.csrf, `hx-post="/ui/apps/media/sync"`, `name="csrf"`, `/static/htmx-2.0.11.min.js`, `/static/sse-2.2.4.js`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if code, _, _ := b.do("POST", "/ui/apps/media/sync", "", nil); code != 403 {
		t.Fatalf("CSRF: %d", code)
	}
	for path, form := range map[string]string{"/ui/apps/media/sync": "", "/ui/apps/media/rollback": "revision=1", "/ui/apps/media/pause": "", "/ui/apps/media/resume": "", "/ui/apps/media/refresh": "", "/ui/hosts/nas/maintenance": "enabled=true"} {
		code, _, body := b.do("POST", path, form, map[string]string{"Content-Type": "application/x-www-form-urlencoded", auth.CSRFHeader: b.csrf, "HX-Request": "true"})
		if code != 200 || !strings.Contains(body, "Action requested") {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}
	if code, _, _ := b.do("GET", "/apps/absent", "", nil); code != 404 {
		t.Errorf("unknown app: %d", code)
	}
	if err := a.AddUser("guest", auth.RoleReadonly, "guest password!"); err != nil {
		t.Fatal(err)
	}
	g := newBrowser(t, srv)
	g.login("guest", "guest password!")
	_, _, body = g.do("GET", "/apps/media?tab=history", "", nil)
	if strings.Contains(body, "hx-post") {
		t.Fatal("readonly action buttons")
	}
	if code, _, _ := g.do("POST", "/ui/apps/media/sync", "csrf="+g.csrf, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != 403 {
		t.Fatalf("readonly action: %d", code)
	}
	_, _, body = g.do("GET", "/settings", "", nil)
	if strings.Contains(body, "Create token") {
		t.Fatal("readonly account management")
	}
	// Ordinary forms also work and carry a CSRF field.
	code, h, _ := b.do("POST", "/ui/apps/media/sync", "csrf="+b.csrf, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if code != 303 || h.Get("Location") != "/apps/media" {
		t.Errorf("form fallback: %d %v", code, h)
	}
}

func TestWebSettings(t *testing.T) {
	srv, _ := newServer(t, Options{})
	b := newBrowser(t, srv)
	b.login("", "secret")
	b.do("GET", "/settings", "", nil)
	post := func(action, form string) string {
		t.Helper()
		code, _, body := b.do("POST", "/ui/settings/"+action, form, map[string]string{"Content-Type": "application/x-www-form-urlencoded", auth.CSRFHeader: b.csrf, "HX-Request": "true"})
		if code != 200 {
			t.Fatalf("%s: %d %s", action, code, body)
		}
		return body
	}
	body := post("token-create", "name=reader&role=readonly")
	if !strings.Contains(body, "ccd_") {
		t.Fatal("token not shown once")
	}
	_, _, body = b.do("GET", "/settings", "", nil)
	if strings.Contains(body, "ccd_") || !strings.Contains(body, "reader") {
		t.Fatal("token list leaks token or omits name")
	}
	post("token-revoke", "name=reader")
	post("user-add", "name=reader&role=readonly&password=long-password")
	post("user-password", "name=reader&password=another-password")
	post("user-delete", "name=reader")
}

func TestWebEventStream(t *testing.T) {
	a := newAuthn(t, "secret")
	st := &webStub{updates: make(chan controller.Update, 1)}
	srv := httptest.NewServer(New(st, Options{Auth: a}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?page=/apps&host=missing", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header)
	}
	sc := bufio.NewScanner(resp.Body)
	read := func(kind string) {
		t.Helper()
		var b strings.Builder
		for sc.Scan() {
			if sc.Text() == "" {
				break
			}
			b.WriteString(sc.Text())
			b.WriteByte('\n')
		}
		body := b.String()
		if !strings.HasPrefix(body, "event: "+kind+"\n") || !strings.Contains(body, `hx-swap-oob="outerHTML"`) || !strings.Contains(body, "No applications match") {
			t.Fatalf("bad SSE frame: %s (%v)", body, sc.Err())
		}
	}
	read("app")
	st.updates <- controller.Update{Kind: "host", Name: "nas"}
	read("host")
	st.updates <- controller.Update{Kind: "operation", Name: "media"}
	read("operation")
}

func TestContainerLogsEndpoint(t *testing.T) {
	srv, _ := newServer(t, Options{})
	for _, path := range []string{"/api/v1/apps/media/containers/other-project/logs", "/api/v1/apps/absent/containers/media-web-1/logs"} {
		code, _ := do(t, srv, "GET", path, "secret", "")
		if code != 404 {
			t.Errorf("foreign container: %d", code)
		}
	}
	for _, tail := range []string{"-1", "10001", "bad"} {
		code, _ := do(t, srv, "GET", "/api/v1/apps/media/containers/media-web-1/logs?tail="+tail, "secret", "")
		if code != 400 {
			t.Errorf("bad tail: %d", code)
		}
	}
	code, body := do(t, srv, "GET", "/api/v1/apps/media/containers/media-web-1/logs?tail=0", "secret", "")
	if code != 200 || !strings.Contains(body, "event: log\ndata: <span>hello &lt;script&gt;") || !strings.Contains(body, "event: end") {
		t.Fatalf("%d %s", code, body)
	}
	code, _ = do(t, srv, "GET", "/api/v1/apps/media/containers/media-web-1/logs", "", "")
	if code != 401 {
		t.Fatal(code)
	}
	resp, err := http.Get(srv.URL + "/static/htmx-2.0.11.min.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	asset, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(asset) < 10000 {
		t.Fatal("missing embedded asset")
	}
}

type followingStub struct {
	stub
	stopped chan struct{}
}

func (s *followingStub) Logs(ctx context.Context, _ string, _ string, _ int, _ bool, w io.Writer) error {
	if _, err := io.WriteString(w, "connected\n"); err != nil {
		return err
	}
	<-ctx.Done()
	close(s.stopped)
	return ctx.Err()
}

func TestReadonlyLogFollowCancellation(t *testing.T) {
	a := newAuthn(t, "secret")
	token, err := a.CreateToken("reader", auth.RoleReadonly, "admin")
	if err != nil {
		t.Fatal(err)
	}
	st := &followingStub{stopped: make(chan struct{})}
	srv := httptest.NewServer(New(st, Options{Auth: a}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/apps/media/containers/media-web-1/logs?follow=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	cancel()
	select {
	case <-st.stopped:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel log backend")
	}
}

type ociValuesStub struct{ stub }

func (s *ociValuesStub) App(name string) (controller.AppStatus, error) {
	a, err := s.stub.App(name)
	a.Source = config.AppSource{Chart: "oci://registry.lan/charts/app", ValuesFrom: &config.GitSource{RepoURL: "https://git.lan/values.git", TargetRevision: "prod"}}
	a.Sync.ChartRef = "oci://registry.lan/charts/app:1.0.0@sha256:abc"
	a.Sync.ValuesRevision = "desired-values-commit"
	a.Sync.DeployedValuesRevision = "deployed-values-commit"
	return a, err
}
func TestWebShowsOCIAndValuesRevisions(t *testing.T) {
	a := newAuthn(t, "secret")
	srv := httptest.NewServer(New(&ociValuesStub{}, Options{Auth: a}))
	defer srv.Close()
	status, body := do(t, srv, "GET", "/apps/media", "secret", "")
	for _, want := range []string{"oci://registry.lan/charts/app:1.0.0@sha256:abc", "https://git.lan/values.git", "desired-values-commit", "deployed-values-commit"} {
		if status != 200 || !strings.Contains(body, want) {
			t.Errorf("%d missing %s: %s", status, want, body)
		}
	}
}
