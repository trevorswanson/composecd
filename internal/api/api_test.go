package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

type stub struct {
	calls []string
}

func (s *stub) Apps() []controller.AppStatus {
	return []controller.AppStatus{{Name: "media", Host: "nas", Sync: controller.SyncStatus{Status: controller.Synced},
		Health: controller.HealthStatus{Status: health.Healthy}}}
}

func (s *stub) App(name string) (controller.AppStatus, error) {
	if name != "media" {
		return controller.AppStatus{}, fmt.Errorf("application %q: %w", name, controller.ErrNotFound)
	}
	return s.Apps()[0], nil
}

func (s *stub) History(name string) ([]controller.Release, error) {
	return []controller.Release{{Revision: 1, Status: "deployed"}}, nil
}

func (s *stub) Operations(app string, limit int) ([]*store.Operation, error) {
	s.calls = append(s.calls, fmt.Sprintf("operations %s %d", app, limit))
	return nil, nil
}

func (s *stub) Operation(id int64) (*store.Operation, error) {
	if id != 7 {
		return nil, controller.ErrNotFound
	}
	return &store.Operation{ID: 7, App: "media", Log: "done"}, nil
}

func (s *stub) LiveObject(_ context.Context, app, id string, all, redact bool) (*controller.Live, error) {
	s.calls = append(s.calls, fmt.Sprintf("live %s %s %v %v", app, id, all, redact))
	if id != "services/web" {
		return nil, controller.ErrNotFound
	}
	return &controller.Live{Object: controller.Object{ID: id}, Projected: "image: nginx:0\n", Desired: "image: nginx:1\n",
		Diff:    "@@ -1 +1 @@\n-image: nginx:0\n+image: nginx:1\n",
		Inspect: []controller.Inspected{{Name: "media-web-1", YAML: "Name: /media-web-1\n", Folded: []controller.Fold{{Path: "GraphDriver", Fields: 2, YAML: "GraphDriver: {}\n"}}}}}, nil
}

func (s *stub) FollowOperation(_ context.Context, id int64, w io.Writer) error {
	op, err := s.Operation(id)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, op.Log+"\n")
	return err
}

func (s *stub) HostStatuses() []controller.HostStatus {
	return []controller.HostStatus{{Status: hosts.Status{Name: "nas", State: hosts.StateUp}, Apps: 1}}
}

func (s *stub) Root() controller.RootStatus {
	return controller.RootStatus{Root: config.Root{RepoURL: "https://git.lan/x.git"}, Revision: "abc"}
}

func (s *stub) Sync(name, user string) (*store.Operation, error) {
	s.calls = append(s.calls, "sync "+name)
	if name == "busy" {
		return nil, controller.ErrBusy
	}
	return &store.Operation{ID: 7, App: name, Phase: store.PhaseRunning}, nil
}

func (s *stub) Rollback(name string, revision int, user string) (*store.Operation, error) {
	s.calls = append(s.calls, fmt.Sprintf("rollback %s %d", name, revision))
	return &store.Operation{ID: 8, App: name}, nil
}

func (s *stub) Pause(name, reason string) error {
	s.calls = append(s.calls, "pause "+name+" "+reason)
	return nil
}

func (s *stub) Resume(name string) error {
	s.calls = append(s.calls, "resume "+name)
	return nil
}

func (s *stub) RefreshApp(name string) error {
	s.calls = append(s.calls, "refresh "+name)
	return nil
}

func (s *stub) Refresh() { s.calls = append(s.calls, "refresh all") }

func (s *stub) RefreshRepos(urls []string) bool {
	for _, u := range urls {
		if u == "https://git.lan/me/homelab.git" {
			s.calls = append(s.calls, "webhook refresh")
			return true
		}
	}
	return false
}

func (s *stub) Windows() []windows.Status {
	return []windows.Status{{Name: "nightly", Kind: "allow", Open: true, Applications: []string{"media"}}}
}

func (s *stub) SetMaintenance(host string, on bool, reason string) error {
	if host != "nas" {
		return fmt.Errorf("host %q: %w", host, controller.ErrNotFound)
	}
	s.calls = append(s.calls, fmt.Sprintf("maintenance %s %v %s", host, on, reason))
	return nil
}

func (s *stub) OperationMetrics() []controller.OperationMetric {
	return []controller.OperationMetric{{Kind: "sync", Trigger: "auto", Phase: "Succeeded", Count: 3, Seconds: 4.5}}
}

// newAuthn returns an authenticator on a fresh store with admin token adminToken.
func newAuthn(t *testing.T, adminToken string) *auth.Authenticator {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &auth.Authenticator{Store: db, AdminToken: adminToken}
}

// newServer serves a stub controller; o.Auth defaults to admin token "secret".
func newServer(t *testing.T, o Options) (*httptest.Server, *stub) {
	t.Helper()
	st := &stub{}
	if o.Auth == nil {
		o.Auth = newAuthn(t, "secret")
	}
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(st, o))
	t.Cleanup(srv.Close)
	return srv, st
}

func do(t *testing.T, srv *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAPI(t *testing.T) {
	srv, st := newServer(t, Options{Version: "v1.2.3"})
	cases := []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{"GET", "/api/v1/apps", "", 200, `"name": "media"`},
		{"GET", "/api/v1/apps/media", "", 200, `"status": "Synced"`},
		{"GET", "/api/v1/apps/nope", "", 404, `"error": "application \"nope\": not found"`},
		{"GET", "/api/v1/apps/media/history", "", 200, `"revision": 1`},
		{"GET", "/api/v1/apps/media/operations?limit=5", "", 200, `[]`},
		{"GET", "/api/v1/apps/media/operations?limit=x", "", 400, `invalid limit`},
		{"POST", "/api/v1/apps/media/sync", "", 202, `"phase": "Running"`},
		{"POST", "/api/v1/apps/busy/sync", "", 409, `already running`},
		{"POST", "/api/v1/apps/media/rollback", `{"revision": 2}`, 202, `"id": 8`},
		{"POST", "/api/v1/apps/media/rollback", `{"revision": -1}`, 400, `negative`},
		{"POST", "/api/v1/apps/media/rollback", `{"rev": 2}`, 400, `unknown field`},
		{"POST", "/api/v1/apps/media/pause", `{"reason": "maintenance"}`, 200, `paused`},
		{"POST", "/api/v1/apps/media/resume", "", 200, `resumed`},
		{"POST", "/api/v1/apps/media/refresh", "", 202, `refresh requested`},
		{"POST", "/api/v1/refresh", "", 202, `refresh requested`},
		{"GET", "/api/v1/operations/7", "", 200, `"log": "done"`},
		{"GET", "/api/v1/operations/9", "", 404, `not found`},
		{"GET", "/api/v1/operations/x", "", 400, `invalid operation id`},
		{"GET", "/api/v1/operations", "", 200, `[]`},
		{"GET", "/api/v1/hosts", "", 200, `"state": "Up"`},
		{"GET", "/api/v1/root", "", 200, `"repoURL": "https://git.lan/x.git"`},
		{"GET", "/api/v1/version", "", 200, `v1.2.3`},
		{"DELETE", "/api/v1/apps/media", "", 405, ``},
	}
	for _, tc := range cases {
		status, body := do(t, srv, tc.method, tc.path, "secret", tc.body)
		if status != tc.status || !strings.Contains(body, tc.contains) {
			t.Errorf("%s %s: %d %s, want %d containing %q", tc.method, tc.path, status, body, tc.status, tc.contains)
		}
	}
	want := "operations media 5;sync media;sync busy;rollback media 2;pause media maintenance (by admin);resume media;refresh media;refresh all;operations  0"
	if got := strings.Join(st.calls, ";"); got != want {
		t.Errorf("calls:\n got %s\nwant %s", got, want)
	}
}

func TestAuth(t *testing.T) {
	srv, st := newServer(t, Options{})
	for _, token := range []string{"", "wrong"} {
		if status, _ := do(t, srv, "GET", "/api/v1/apps", token, ""); status != 401 {
			t.Errorf("token %q: status %d", token, status)
		}
	}
	if status, _ := do(t, srv, "POST", "/api/v1/apps/media/sync", "wrong", ""); status != 401 || len(st.calls) != 0 {
		t.Errorf("unauthorized sync ran: %d %v", status, st.calls)
	}
	if status, body := do(t, srv, "GET", "/healthz", "", ""); status != 200 || body != "ok\n" {
		t.Errorf("healthz: %d %q", status, body)
	}

	// No token configured means nothing is authorized.
	locked, _ := newServer(t, Options{Auth: newAuthn(t, "")})
	if status, _ := do(t, locked, "GET", "/api/v1/apps", "", ""); status != 401 {
		t.Errorf("empty token: status %d", status)
	}
	open, _ := newServer(t, Options{Auth: &auth.Authenticator{Disabled: true}})
	if status, _ := do(t, open, "GET", "/api/v1/apps", "", ""); status != 200 {
		t.Errorf("auth disabled: status %d", status)
	}
}

// browser is an HTTP client that keeps cookies and doesn't follow redirects.
type browser struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	csrf   string
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, srv: srv, client: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path, body string, headers map[string]string) (int, http.Header, string) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.srv.URL+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if m := regexp.MustCompile(`name="csrf-token" content="([0-9a-f]+)"`).FindStringSubmatch(string(out)); m != nil {
		b.csrf = m[1]
	}
	return resp.StatusCode, resp.Header, string(out)
}

func (b *browser) login(user, password string) string {
	b.t.Helper()
	form := url.Values{"user": {user}, "password": {password}}.Encode()
	_, h, _ := b.do("POST", "/login", form, map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": b.srv.URL})
	return h.Get("Location")
}

func TestSessions(t *testing.T) {
	authn := newAuthn(t, "secret")
	if err := authn.AddUser("trevor", auth.RoleAdmin, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := authn.AddUser("guest", auth.RoleReadonly, "guest password!"); err != nil {
		t.Fatal(err)
	}
	srv, st := newServer(t, Options{Auth: authn, Version: "v1"})

	b := newBrowser(t, srv)
	status, _, body := b.do("GET", "/", "", nil)
	if status != 401 || !strings.Contains(body, `name="password"`) || strings.Contains(body, "<strong>media</strong>") || strings.Contains(body, "identity provider") {
		t.Errorf("sign-in page: %d\n%s", status, body)
	}
	if loc := b.login("trevor", "wrong password"); loc != "/?error=failed" {
		t.Errorf("bad password: %q", loc)
	}
	if _, _, body := b.do("GET", "/?error=failed", "", nil); !strings.Contains(body, "aren&#39;t valid") {
		t.Errorf("error message missing:\n%s", body)
	}
	// A cross-site form can't sign the browser in.
	status, _, _ = b.do("POST", "/login", "user=trevor&password=correct+horse+battery",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://evil.example"})
	if status != 403 {
		t.Errorf("cross-site login: %d", status)
	}

	if loc := b.login("trevor", "correct horse battery"); loc != "/" {
		t.Fatalf("login: %q", loc)
	}
	status, _, body = b.do("GET", "/", "", nil)
	for _, want := range []string{"<strong>media</strong>", `class="pill synced"`, `class="pill healthy"`, "<strong>trevor</strong><span>admin", "Sign out"} {
		if status != 200 || !strings.Contains(body, want) {
			t.Errorf("status page lacks %q:\n%s", want, body)
		}
	}
	if b.csrf == "" {
		t.Fatal("no CSRF token on the page")
	}
	if status, _, body := b.do("GET", "/api/v1/me", "", nil); status != 200 || !strings.Contains(body, `"via": "password"`) {
		t.Errorf("me: %d %s", status, body)
	}

	// The session acts only with its CSRF token.
	if status, _, _ := b.do("POST", "/api/v1/apps/media/sync", "", nil); status != 403 || len(st.calls) != 0 {
		t.Errorf("session POST without CSRF: %d %v", status, st.calls)
	}
	if status, _, _ := b.do("POST", "/api/v1/apps/media/sync", "", map[string]string{auth.CSRFHeader: b.csrf}); status != 202 {
		t.Errorf("session POST with CSRF: %d", status)
	}

	// Sign out needs the CSRF token too, then the session is gone.
	if status, _, _ := b.do("POST", "/logout", "", nil); status != 403 {
		t.Errorf("logout without CSRF: %d", status)
	}
	b.do("POST", "/logout", "csrf="+b.csrf, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if status, _, _ := b.do("GET", "/api/v1/me", "", nil); status != 401 {
		t.Errorf("after logout: %d", status)
	}

	// A read-only user can look but not act.
	g := newBrowser(t, srv)
	g.login("guest", "guest password!")
	g.do("GET", "/", "", nil)
	if status, _, _ := g.do("GET", "/api/v1/apps", "", nil); status != 200 {
		t.Errorf("readonly read: %d", status)
	}
	if status, _, body := g.do("POST", "/api/v1/apps/media/sync", "", map[string]string{auth.CSRFHeader: g.csrf}); status != 403 || !strings.Contains(body, "admin role") {
		t.Errorf("readonly sync: %d %s", status, body)
	}
	// ...but may change their own password, not someone else's.
	if status, _, _ := g.do("PUT", "/api/v1/users/trevor/password", `{"password": "taken over!!"}`, map[string]string{auth.CSRFHeader: g.csrf}); status != 403 {
		t.Errorf("changing another user's password: %d", status)
	}
	if status, _, _ := g.do("PUT", "/api/v1/users/guest/password", `{"password": "a new guest password"}`, map[string]string{auth.CSRFHeader: g.csrf}); status != 200 {
		t.Errorf("changing own password: %d", status)
	}
	if status, _, _ := g.do("GET", "/api/v1/me", "", nil); status != 401 {
		t.Errorf("a password change should sign the user out: %d", status)
	}

	// Signing in with an API token instead of a password.
	tb := newBrowser(t, srv)
	if loc := tb.login("", "secret"); loc != "/" {
		t.Errorf("admin token login: %q", loc)
	}
	if status, _, body := tb.do("GET", "/api/v1/me", "", nil); status != 200 || !strings.Contains(body, `"via": "admin-token"`) {
		t.Errorf("token session: %d %s", status, body)
	}
}

func TestAccounts(t *testing.T) {
	srv, _ := newServer(t, Options{})
	status, body := do(t, srv, "POST", "/api/v1/tokens", "secret", `{"name": "grafana", "role": "readonly"}`)
	var created struct{ Token string }
	_ = json.Unmarshal([]byte(body), &created)
	if status != 201 || !strings.HasPrefix(created.Token, "ccd_") {
		t.Fatalf("create token: %d %s", status, body)
	}
	if status, _ := do(t, srv, "POST", "/api/v1/tokens", "secret", `{"name": "grafana", "role": "readonly"}`); status != 409 {
		t.Errorf("duplicate token: %d", status)
	}
	if status, _ := do(t, srv, "POST", "/api/v1/tokens", "secret", `{"name": "x", "role": "owner"}`); status != 400 {
		t.Errorf("bad role: %d", status)
	}
	if status, body := do(t, srv, "GET", "/api/v1/tokens", "secret", ""); status != 200 || !strings.Contains(body, `"createdBy": "admin"`) || strings.Contains(body, "Hash") {
		t.Errorf("list tokens: %d %s", status, body)
	}
	// The new token reads but can't act or manage accounts.
	if status, _ := do(t, srv, "GET", "/api/v1/apps", created.Token, ""); status != 200 {
		t.Errorf("readonly token read: %d", status)
	}
	for _, c := range [][2]string{{"POST", "/api/v1/apps/media/sync"}, {"GET", "/api/v1/tokens"}, {"POST", "/api/v1/hosts/nas/maintenance"}} {
		if status, _ := do(t, srv, c[0], c[1], created.Token, ""); status != 403 {
			t.Errorf("readonly %s %s: %d", c[0], c[1], status)
		}
	}
	if status, _ := do(t, srv, "DELETE", "/api/v1/tokens/grafana", "secret", ""); status != 200 {
		t.Errorf("revoke: %d", status)
	}
	if status, _ := do(t, srv, "GET", "/api/v1/apps", created.Token, ""); status != 401 {
		t.Errorf("revoked token: %d", status)
	}
	if status, _ := do(t, srv, "DELETE", "/api/v1/tokens/grafana", "secret", ""); status != 404 {
		t.Errorf("revoking twice: %d", status)
	}

	if status, body := do(t, srv, "POST", "/api/v1/users", "secret", `{"name": "kid", "role": "readonly", "password": "short"}`); status != 400 || !strings.Contains(body, "at least 10") {
		t.Errorf("short password: %d %s", status, body)
	}
	if status, _ := do(t, srv, "POST", "/api/v1/users", "secret", `{"name": "kid", "role": "readonly", "password": "a long password"}`); status != 201 {
		t.Errorf("add user: %d", status)
	}
	if status, body := do(t, srv, "GET", "/api/v1/users", "secret", ""); status != 200 || !strings.Contains(body, `"name": "kid"`) || strings.Contains(body, "assword") {
		t.Errorf("list users: %d %s", status, body)
	}
	if status, _ := do(t, srv, "PUT", "/api/v1/users/kid/password", "secret", `{"password": "another long one"}`); status != 200 {
		t.Errorf("admin sets a password: %d", status)
	}
	if status, _ := do(t, srv, "DELETE", "/api/v1/users/kid", "secret", ""); status != 200 {
		t.Errorf("delete user: %d", status)
	}
	if status, _ := do(t, srv, "DELETE", "/api/v1/users/kid", "secret", ""); status != 404 {
		t.Errorf("delete twice: %d", status)
	}
}

func TestOIDCRoutes(t *testing.T) {
	authn := newAuthn(t, "secret")
	o, err := auth.NewOIDC(config.OIDC{Issuer: "http://127.0.0.1:1", ClientID: "composecd", RedirectURL: "https://cd.lan/sso/done", AdminGroups: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	authn.OIDC = o
	srv, _ := newServer(t, Options{Auth: authn})
	b := newBrowser(t, srv)
	if _, _, body := b.do("GET", "/", "", nil); !strings.Contains(body, `href="/auth/oidc"`) {
		t.Errorf("no OIDC button:\n%s", body)
	}
	// The provider is unreachable: the user is sent back with a message.
	if _, h, _ := b.do("GET", "/auth/oidc", "", nil); h.Get("Location") != "/?error=oidc" {
		t.Errorf("unreachable provider: %q", h.Get("Location"))
	}
	// The callback is served at the redirect URL's path.
	if _, h, _ := b.do("GET", "/sso/done?state=x&code=y", "", nil); h.Get("Location") != "/?error=oidc" {
		t.Errorf("callback without a sign-in: %q", h.Get("Location"))
	}
}

func TestPhase2Endpoints(t *testing.T) {
	srv, st := newServer(t, Options{})
	if status, body := do(t, srv, "GET", "/api/v1/windows", "secret", ""); status != 200 || !strings.Contains(body, `"open": true`) {
		t.Errorf("windows: %d %s", status, body)
	}
	if status, body := do(t, srv, "POST", "/api/v1/hosts/nas/maintenance", "secret", `{"enabled": true, "reason": "disks"}`); status != 200 || !strings.Contains(body, `"maintenance": true`) {
		t.Errorf("maintenance: %d %s", status, body)
	}
	if status, _ := do(t, srv, "POST", "/api/v1/hosts/pi/maintenance", "secret", `{"enabled": true}`); status != 404 {
		t.Errorf("unknown host: %d", status)
	}
	if strings.Join(st.calls, ";") != "maintenance nas true disks (by admin)" {
		t.Errorf("calls = %v", st.calls)
	}

	status, body := do(t, srv, "GET", "/metrics", "", "")
	for _, want := range []string{
		`# TYPE composecd_app_sync_status gauge`,
		`composecd_app_sync_status{app="media",host="nas",status="Synced"} 1`,
		`composecd_app_health_status{app="media",host="nas",status="Healthy"} 1`,
		`composecd_host_up{host="nas"} 1`,
		`composecd_operations_total{kind="sync",trigger="auto",phase="Succeeded"} 3`,
		`composecd_operation_duration_seconds_total{kind="sync",trigger="auto",phase="Succeeded"} 4.5`,
		`composecd_sync_window_open{window="nightly",kind="allow"} 1`,
		`composecd_root_error 0`,
	} {
		if status != 200 || !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
}

func TestWebhook(t *testing.T) {
	push := `{"repository": {"clone_url": "https://git.lan/me/homelab.git", "ssh_url": "git@git.lan:me/homelab.git"}}`
	other := `{"repository": {"clone_url": "https://git.lan/me/other.git"}}`
	gitlab := `{"project": {"git_http_url": "https://git.lan/me/homelab.git"}}`
	sign := func(body, secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	post := func(srv *httptest.Server, body string, headers map[string]string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/webhook", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	off, _ := newServer(t, Options{})
	if status, _ := post(off, push, map[string]string{"X-GitHub-Event": "push"}); status != 404 {
		t.Errorf("webhook without a secret: %d", status)
	}

	srv, st := newServer(t, Options{WebhookSecret: "hook-secret"})
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
		says    string
	}{
		{"github push", push, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign(push, "hook-secret")}, 202, "refresh requested"},
		{"github ping", push, map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": "sha256=" + sign(push, "hook-secret")}, 200, "pong"},
		{"gitea push", push, map[string]string{"X-Gitea-Event": "push", "X-Gitea-Signature": sign(push, "hook-secret")}, 202, "refresh requested"},
		{"forgejo other repo", other, map[string]string{"X-Forgejo-Event": "push", "X-Forgejo-Signature": sign(other, "hook-secret")}, 200, "no application uses"},
		{"gitlab push", gitlab, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "hook-secret"}, 202, "refresh requested"},
		{"issue event", push, map[string]string{"X-GitHub-Event": "issues", "X-Hub-Signature-256": "sha256=" + sign(push, "hook-secret")}, 200, "ignored event"},
		{"wrong secret", push, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign(push, "nope")}, 401, "invalid"},
		{"no signature", push, map[string]string{"X-GitHub-Event": "push"}, 401, "invalid"},
		{"wrong gitlab token", gitlab, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "nope"}, 401, "invalid"},
		{"tampered body", push + " ", map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign(push, "hook-secret")}, 401, "invalid"},
	}
	for _, tc := range cases {
		status, body := post(srv, tc.body, tc.headers)
		if status != tc.status || !strings.Contains(body, tc.says) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, status, body, tc.status, tc.says)
		}
	}
	if n := strings.Count(strings.Join(st.calls, ";"), "webhook refresh"); n != 3 {
		t.Errorf("refreshes = %d (%v)", n, st.calls)
	}
}

func (s *stub) Subscribe() (<-chan controller.Update, func()) {
	return make(chan controller.Update), func() {}
}
func (s *stub) Logs(_ context.Context, name, container string, _ int, _ bool, w io.Writer) error {
	if name != "media" || container != "media-web-1" {
		return controller.ErrNotFound
	}
	_, err := io.WriteString(w, "hello <script>\n")
	return err
}
func (s *stub) Mask(_ string, text string) string {
	return strings.ReplaceAll(text, "secret-value", "••••••")
}

func (s *stub) ApplicationSets() []controller.ApplicationSetStatus {
	return []controller.ApplicationSetStatus{{Name: "stacks", Generated: 1, Ready: true, Applications: []string{"media"}}}
}
