package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

func (s *stub) HostStatuses() []controller.HostStatus {
	return []controller.HostStatus{{Status: hosts.Status{Name: "nas", State: hosts.StateUp}, Apps: 1}}
}

func (s *stub) Root() controller.RootStatus {
	return controller.RootStatus{Root: config.Root{RepoURL: "https://git.lan/x.git"}, Revision: "abc"}
}

func (s *stub) Sync(name string) (*store.Operation, error) {
	s.calls = append(s.calls, "sync "+name)
	if name == "busy" {
		return nil, controller.ErrBusy
	}
	return &store.Operation{ID: 7, App: name, Phase: store.PhaseRunning}, nil
}

func (s *stub) Rollback(name string, revision int) (*store.Operation, error) {
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

func newServer(t *testing.T, o Options) (*httptest.Server, *stub) {
	t.Helper()
	st := &stub{}
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
	srv, st := newServer(t, Options{Token: "secret", Version: "v1.2.3"})
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
	want := "operations media 5;sync media;sync busy;rollback media 2;pause media maintenance;resume media;refresh media;refresh all;operations  0"
	if got := strings.Join(st.calls, ";"); got != want {
		t.Errorf("calls:\n got %s\nwant %s", got, want)
	}
}

func TestAuth(t *testing.T) {
	srv, st := newServer(t, Options{Token: "secret"})
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
	locked, _ := newServer(t, Options{})
	if status, _ := do(t, locked, "GET", "/api/v1/apps", "", ""); status != 401 {
		t.Errorf("empty token: status %d", status)
	}
	open, _ := newServer(t, Options{AuthDisabled: true})
	if status, _ := do(t, open, "GET", "/api/v1/apps", "", ""); status != 200 {
		t.Errorf("auth disabled: status %d", status)
	}
}

func TestStatusPageLogin(t *testing.T) {
	srv, st := newServer(t, Options{Token: "secret", Version: "v1"})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(body), `name="token"`) || strings.Contains(string(body), "<strong>media</strong>") {
		t.Errorf("login page: %d\n%s", resp.StatusCode, body)
	}

	resp, err = client.PostForm(srv.URL+"/login", url.Values{"token": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/?failed" || len(resp.Cookies()) != 0 {
		t.Errorf("bad login: %q %v", loc, resp.Cookies())
	}

	resp, err = client.PostForm(srv.URL+"/login", url.Values{"token": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookies = %+v", cookies)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"<strong>media</strong>", `class="badge synced"`, `class="badge healthy"`, `class="badge up"`, "nas",
		"<strong>nightly</strong>", `<span class="badge outofsync">open</span>`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("status page lacks %q:\n%s", want, body)
		}
	}

	// The cookie is not enough to trigger an operation.
	req, _ = http.NewRequest("POST", srv.URL+"/api/v1/apps/media/sync", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || len(st.calls) != 0 {
		t.Errorf("cookie POST: %d %v", resp.StatusCode, st.calls)
	}
	// But it reads the API.
	req, _ = http.NewRequest("GET", srv.URL+"/api/v1/apps", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var apps []controller.AppStatus
	_ = json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(apps) != 1 {
		t.Errorf("cookie GET: %d %v", resp.StatusCode, apps)
	}
}

func TestPhase2Endpoints(t *testing.T) {
	srv, st := newServer(t, Options{Token: "secret"})
	if status, body := do(t, srv, "GET", "/api/v1/windows", "secret", ""); status != 200 || !strings.Contains(body, `"open": true`) {
		t.Errorf("windows: %d %s", status, body)
	}
	if status, body := do(t, srv, "POST", "/api/v1/hosts/nas/maintenance", "secret", `{"enabled": true, "reason": "disks"}`); status != 200 || !strings.Contains(body, `"maintenance": true`) {
		t.Errorf("maintenance: %d %s", status, body)
	}
	if status, _ := do(t, srv, "POST", "/api/v1/hosts/pi/maintenance", "secret", `{"enabled": true}`); status != 404 {
		t.Errorf("unknown host: %d", status)
	}
	if strings.Join(st.calls, ";") != "maintenance nas true disks" {
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

	off, _ := newServer(t, Options{Token: "secret"})
	if status, _ := post(off, push, map[string]string{"X-GitHub-Event": "push"}); status != 404 {
		t.Errorf("webhook without a secret: %d", status)
	}

	srv, st := newServer(t, Options{Token: "secret", WebhookSecret: "hook-secret"})
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
