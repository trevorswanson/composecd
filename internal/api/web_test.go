package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/appset"
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
	a.Sync.Drift = []action.ServiceDrift{{Service: "cache", Reason: action.DriftOrphaned, Containers: []string{"media-cache-1"}}}
	a.Sync.RenderedAt = time.Now()
	a.Objects = []controller.Object{
		{ID: "services/web", Kind: controller.KindService, Name: "web", Image: "nginx:1", Sync: controller.ObjectOutOfSync, Desired: "image: nginx:1\n", Uses: []string{"networks/default"}},
		{ID: "networks/default", Kind: controller.KindNetwork, Name: "default", Sync: controller.ObjectSynced},
		{ID: "containers/media-web-1", Kind: controller.KindContainer, Name: "media-web-1", Parent: "services/web", Sync: controller.ObjectSynced},
	}
	a.Images = []controller.ImageStatus{
		{Service: "web", Image: "nginx:1", Ref: "docker.io/library/nginx:1", Pin: controller.PinComposecd, Digest: "sha256:bbbbbbbbbbbbbbbb",
			Deployed: "sha256:aaaaaaaaaaaaaaaa", Latest: "sha256:bbbbbbbbbbbbbbbb", UpdateAvailable: true, Applicable: true},
		{Service: "app", Image: "ghcr.io/me/app:1.27.1", Ref: "ghcr.io/me/app:1.27.3", Strategy: "semver", Constraint: "~1.27", Tag: "1.27.1",
			Candidate: "1.27.3", Pin: controller.PinComposecd, Digest: "sha256:eeeeeeeeeeeeeeee", Latest: "sha256:eeeeeeeeeeeeeeee", UpdateAvailable: true, Applicable: true},
		{Service: "db", Image: "postgres:16@sha256:cccccccccccccccc", Ref: "docker.io/library/postgres:16", Pin: controller.PinGit,
			Digest: "sha256:cccccccccccccccc", Latest: "sha256:dddddddddddddddd", UpdateAvailable: true, Note: "the digest is pinned in Git; change it there to update"},
		{Service: "worker", Image: "ghcr.io/me/worker:2.0.0", Strategy: "semver", Tag: "2.0.0", Candidate: "2.1.0", WriteBack: "git", UpdateAvailable: true,
			Held: "the tag is set in an encrypted file", File: "apps/media/values.yaml", Key: "worker.tag", Note: "held: the tag is set in an encrypted file"},
		{Service: "proxy", Image: "caddy:2.8.0", Strategy: "semver", Tag: "2.8.0", Candidate: "2.8.4", WriteBack: "git", Diverged: true, Applicable: true,
			Decision: "memory", DecidedBy: "admin", Note: "applied in memory by admin, diverging from Git, which says 2.8.0"},
	}
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
		"/apps?label=missing": "No applications match", "/hosts": "Start maintenance", "/hosts/nas": "traefik-proxy-1", "/windows": "nightly",
		"/operations": "operator", "/settings": "Create token",
		"/apps/media": "Sync status", "/ui/apps/media/details": "Desired revision", "/apps/media?tab=containers": "media-web-1",
		"/apps/media?tab=graph": `class="graph-data"`, "/ui/apps/media/objects/services/web": "nginx:1", "/ui/apps/media/objects/services/web?view=desired": "image: nginx:1",
		"/ui/apps/media/operations/7":                   `sse-connect="/api/v1/apps/media/operations/7/log"`,
		"/ui/apps/media/objects/services/web?view=live": "GraphDriver", "/ui/apps/media/objects/services/web?view=diff": `<span class="removed">-image: nginx:0</span>`,
		"/apps/media?tab=diff&mode=objects": "services/web", "/apps/media?tab=diff&mode=objects&synced=1": "networks/default",
		"/apps/media?tab=diff": "services/web", "/apps/media?tab=diff&existing": "<strong>cache</strong>: not in Git, would be removed", "/apps/media?tab=history": "Roll back",
		"/apps/media?tab=operations": "Output",
		"/apps/media?tab=images":     `disabled title="the digest is pinned in Git; change it there to update">Can't apply</button>`,
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
	// An operation's result stays to show its output; anything else is a toast
	// that leaves a result already showing in place.
	for path, form := range map[string][3]string{
		"/ui/apps/media/sync":           {"", `<strong>Sync #7 running</strong>`, ""},
		"/ui/apps/media/rollback":       {"revision=1", `sse-connect="/api/v1/apps/media/operations/8/log"`, ""},
		"/ui/apps/media/pause":          {"", "Paused automated sync of media", "none"},
		"/ui/apps/media/resume":         {"", "Resumed automated sync of media", "none"},
		"/ui/apps/media/refresh":        {"", "Refreshed media", "none"},
		"/ui/apps/media/check-images":   {"", "Checked 2 images; docker.io/library/nginx:1 has a new digest", "none"},
		"/ui/apps/media/image-decision": {"service=web&choice=memory", "Applying 1.27.3 to web in memory; it diverges from Git", "none"},
		"/ui/hosts/nas/maintenance":     {"enabled=true", "Started maintenance on nas", "none"},
		"/ui/hosts/nas/rescan":          {"", "Rescanned nas", "none"},
		"/ui/root/refresh":              {"", "Fetched the root repository at abc", "none"},
	} {
		code, h, body := b.do("POST", path, form[0], map[string]string{"Content-Type": "application/x-www-form-urlencoded", auth.CSRFHeader: b.csrf, "HX-Request": "true"})
		if code != 200 || !strings.Contains(body, form[1]) || h.Get("HX-Reswap") != form[2] {
			t.Errorf("%s: %d %v %s", path, code, h, body)
		}
	}
	// A registry that didn't answer fails the check, naming the image.
	if code, _, body := b.do("POST", "/ui/root/check-images", "", map[string]string{"Content-Type": "application/x-www-form-urlencoded", auth.CSRFHeader: b.csrf, "HX-Request": "true"}); code != 500 || !strings.Contains(body, "ghcr.io/me/private:1: unauthorized") {
		t.Errorf("failed image check: %d %s", code, body)
	}
	if !slices.Contains(st.calls, "check-images media") || !slices.Contains(st.calls, "check-images ") {
		t.Errorf("image checks not requested: %v", st.calls)
	}
	_, _, body = b.do("GET", "/apps/media?tab=images", "", nil)
	if !strings.Contains(body, "sha256:bbbbbbbbbbbb") || !strings.Contains(body, `hx-post="/ui/apps/media/sync"`) || !strings.Contains(body, ">images<span class=\"count warn\">4</span>") ||
		!strings.Contains(body, "semver <code>~1.27</code>") || !strings.Contains(body, "1.27.1 → <strong>1.27.3</strong>") {
		t.Errorf("images tab: %s", body)
	}
	// A held update names the file to fix and offers an admin the decision;
	// one applied in memory says it diverges.
	if !strings.Contains(body, "the tag is set in an encrypted file") || !strings.Contains(body, "set <code>worker.tag</code> in <code>apps/media/values.yaml</code>") ||
		!strings.Contains(body, `name="choice" value="memory"`) || !strings.Contains(body, `name="choice" value="dismiss"`) || !strings.Contains(body, "Diverged from Git") {
		t.Errorf("write-back on the images tab: %s", body)
	}
	if code, _, body := b.do("POST", "/ui/apps/media/image-decision", "service=web&choice=yolo", map[string]string{"Content-Type": "application/x-www-form-urlencoded",
		auth.CSRFHeader: b.csrf, "HX-Request": "true"}); code != 400 || !strings.Contains(body, "invalid choice") {
		t.Errorf("bad decision: %d %s", code, body)
	}
	if code, _, _ := b.do("POST", "/ui/apps/media/image-decision", "service=db&choice=dismiss", map[string]string{"Content-Type": "application/x-www-form-urlencoded",
		auth.CSRFHeader: b.csrf, "HX-Request": "true"}); code != 409 {
		t.Errorf("decision on an update that isn't held: %d", code)
	}
	if !slices.Contains(st.calls, "refresh all") {
		t.Errorf("root refresh not requested: %v", st.calls)
	}
	for _, path := range []string{"/apps/absent", "/hosts/absent"} {
		if code, _, _ := b.do("GET", path, "", nil); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _, _ := b.do("POST", "/ui/hosts/absent/rescan", "", map[string]string{auth.CSRFHeader: b.csrf, "HX-Request": "true"}); code != 404 {
		t.Errorf("rescan unknown host: %d", code)
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
	// A readonly user sees a held update but can't decide it.
	if _, _, body = g.do("GET", "/apps/media?tab=images", "", nil); !strings.Contains(body, "the tag is set in an encrypted file") || strings.Contains(body, "Apply in memory") {
		t.Errorf("readonly images tab: %s", body)
	}
	if code, _, _ := g.do("POST", "/ui/apps/media/image-decision", "csrf="+g.csrf+"&service=web&choice=memory", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != 403 {
		t.Errorf("readonly decision: %d", code)
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
	if !strings.Contains(body, "ccd_") || !strings.Contains(body, "Created the token reader") {
		t.Fatal("token not shown once")
	}
	_, _, body = b.do("GET", "/settings", "", nil)
	if strings.Contains(body, "ccd_") || !strings.Contains(body, "reader") {
		t.Fatal("token list leaks token or omits name")
	}
	// Settings has no live updates, so each result re-renders it.
	if body = post("token-revoke", "name=reader"); !strings.Contains(body, "Revoked the token reader") || !strings.Contains(body, "No named tokens.") {
		t.Errorf("token-revoke: %s", body)
	}
	if body = post("user-add", "name=reader&role=readonly&password=long-password"); !strings.Contains(body, "Added reader as readonly") || !strings.Contains(body, `id="pw-reader"`) || !strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Errorf("user-add: %s", body)
	}
	if body = post("user-password", "name=reader&password=another-password"); !strings.Contains(body, "Changed reader&#39;s password") {
		t.Errorf("user-password: %s", body)
	}
	if body = post("user-delete", "name=reader"); !strings.Contains(body, "Deleted reader") || strings.Contains(body, `id="pw-reader"`) {
		t.Errorf("user-delete: %s", body)
	}
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

func TestOperationLogEndpoint(t *testing.T) {
	srv, _ := newServer(t, Options{})
	code, body := do(t, srv, "GET", "/api/v1/apps/media/operations/7/log", "secret", "")
	if code != 200 || !strings.Contains(body, "event: log\ndata: <span>done") || !strings.Contains(body, "event: end") {
		t.Fatalf("%d %s", code, body)
	}
	for path, want := range map[string]int{
		"/api/v1/apps/other/operations/7/log": 404, "/api/v1/apps/media/operations/9/log": 404, "/api/v1/apps/media/operations/x/log": 400,
	} {
		if code, _ := do(t, srv, "GET", path, "secret", ""); code != want {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ := do(t, srv, "GET", "/api/v1/apps/media/operations/7/log", "", ""); code != 401 {
		t.Fatal(code)
	}
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
	status, body := do(t, srv, "GET", "/ui/apps/media/details", "secret", "")
	for _, want := range []string{"oci://registry.lan/charts/app:1.0.0@sha256:abc", "https://git.lan/values.git", "desired-values-commit", "deployed-values-commit"} {
		if status != 200 || !strings.Contains(body, want) {
			t.Errorf("%d missing %s: %s", status, want, body)
		}
	}
}

type appsetErrorStub struct{ stub }

func (s *appsetErrorStub) ApplicationSets() []controller.ApplicationSetStatus {
	return []controller.ApplicationSetStatus{{Name: "stacks", Generated: 0, Previous: 2, Applications: []string{"media"}, Error: "template error <script>"}}
}

type appsetAppErrorStub struct{ stub }

func (s *appsetAppErrorStub) ApplicationSets() []controller.ApplicationSetStatus {
	return []controller.ApplicationSetStatus{{Name: "stacks", Generated: 2, Ready: true, Applications: []string{"media", "web"},
		Warnings:  []string{"skipped 1 matched directory with no Chart.yaml at the generated source path: charts/<x>"},
		AppErrors: map[string]string{"web": "rendering: charts/web/templates/a.yaml:3: <bad>"}}}
}

func TestApplicationSetsPageShowsAppErrorsAndWarnings(t *testing.T) {
	a := newAuthn(t, "secret")
	srv := httptest.NewServer(New(&appsetAppErrorStub{}, Options{Auth: a}))
	defer srv.Close()
	code, body := do(t, srv, "GET", "/api/v1/applicationsets", "secret", "")
	if code != 200 || !strings.Contains(body, `"appErrors"`) || !strings.Contains(body, `"warnings"`) {
		t.Fatalf("%d %s", code, body)
	}
	code, body = do(t, srv, "GET", "/applicationsets", "secret", "")
	for _, want := range []string{"1 failing", `<a href="/apps/web">web</a>: rendering: charts/web/templates/a.yaml:3: &lt;bad&gt;`, "charts/&lt;x&gt;"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%d missing %s: %s", code, want, body)
		}
	}
	if strings.Contains(body, "pill ready") {
		t.Error("a set with failing applications is shown as ready")
	}
}

func TestApplicationSetsAPIAndPage(t *testing.T) {
	a := newAuthn(t, "secret")
	token, err := a.CreateToken("reader", auth.RoleReadonly, "admin")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(&appsetErrorStub{}, Options{Auth: a}))
	defer srv.Close()
	code, body := do(t, srv, "GET", "/api/v1/applicationsets", token, "")
	if code != 200 || !strings.Contains(body, `"name": "stacks"`) || !strings.Contains(body, `"previous": 2`) {
		t.Fatalf("%d %s", code, body)
	}
	code, body = do(t, srv, "GET", "/applicationsets", token, "")
	for _, want := range []string{"ApplicationSets", "stacks", "not applied", "was 2", "template error &lt;script&gt;", "/apps/media"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%d missing %s: %s", code, want, body)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("unescaped generator error")
	}
	if code, _ := do(t, srv, "GET", "/api/v1/applicationsets", "", ""); code != 401 {
		t.Fatal(code)
	}
	// The new page participates in authenticated SSE panel swaps too.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?page=/applicationsets", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scan := bufio.NewScanner(resp.Body)
	var frame strings.Builder
	for scan.Scan() {
		if scan.Text() == "" {
			break
		}
		frame.WriteString(scan.Text())
	}
	if !strings.Contains(frame.String(), `hx-swap-oob="outerHTML"`) || !strings.Contains(frame.String(), "stacks") {
		t.Fatalf("bad appset SSE: %s", &frame)
	}
}

type countingDiscovery struct {
	webStub
	calls atomic.Int32
}

func (s *countingDiscovery) Unmanaged(ctx context.Context, host string) (*controller.Unmanaged, error) {
	s.calls.Add(1)
	return s.webStub.Unmanaged(ctx, host)
}

// Host pages serve discovery from a cache that background scans refresh, so
// neither page loads nor live updates wait on every host's releases and projects.
func TestHostsEventsReuseDiscovery(t *testing.T) {
	a := newAuthn(t, "secret")
	st := &countingDiscovery{webStub: webStub{updates: make(chan controller.Update, 1)}}
	srv := httptest.NewServer(New(st, Options{Auth: a}))
	defer srv.Close()
	// The first load waits briefly for a quick host's first scan.
	if code, body := do(t, srv, "GET", "/hosts/nas", "secret", ""); code != 200 || !strings.Contains(body, "external") {
		t.Fatal(code, body)
	}
	if code, body := do(t, srv, "GET", "/hosts", "secret", ""); code != 200 || !strings.Contains(body, "1 release · 1 Compose project") {
		t.Fatal(code, body)
	}
	if got := st.calls.Load(); got != 1 {
		t.Fatalf("page loads ran discovery %d times", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?page=/hosts/nas", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	frames := 0
	next := func() {
		for sc.Scan() {
			if sc.Text() == "" {
				frames++
				return
			}
		}
		t.Fatal("missing frame", sc.Err())
	}
	next()
	// (A frame the same as the last isn't sent, so this one's kind differs.)
	st.updates <- controller.Update{Kind: "host", Name: "nas"}
	next()
	if got := st.calls.Load(); got != 1 {
		t.Fatalf("live updates ran discovery %d more times", got-1)
	}
	// A rescan runs discovery again; the open page gets a frame only if
	// what it shows changed, which with this stub it doesn't.
	if code, body := do(t, srv, "POST", "/ui/hosts/nas/rescan", "secret", ""); code != 200 {
		t.Fatal(code, body)
	}
	for st.calls.Load() < 2 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if got := st.calls.Load(); got != 2 {
		t.Fatalf("rescan ran discovery %d times", got-1)
	}
}

// selfStub's only app, and its only release on nas, run composecd itself.
type selfStub struct{ stub }

func (s *selfStub) Apps() []controller.AppStatus {
	apps := s.stub.Apps()
	apps[0].Self = true
	return apps
}
func (s *selfStub) App(name string) (controller.AppStatus, error) {
	if _, err := s.stub.App(name); err != nil {
		return controller.AppStatus{}, err
	}
	return s.Apps()[0], nil
}
func (s *selfStub) Unmanaged(_ context.Context, _ string) (*controller.Unmanaged, error) {
	return &controller.Unmanaged{Releases: []controller.UnmanagedRelease{{Name: "media", Revision: 1, Orphaned: true, Self: true}}}, nil
}

func TestWebDisablesActionsOnSelf(t *testing.T) {
	a := newAuthn(t, "secret")
	srv := httptest.NewServer(New(&selfStub{}, Options{Auth: a}))
	defer srv.Close()
	for path, want := range map[string]string{
		"/apps":                   `<span class="tag self"`,
		"/apps/media":             `disabled title="composecd can't update itself here`,
		"/apps/media?tab=history": "composecd can't roll itself back here",
		"/hosts/nas":              "composecd itself",
	} {
		status, body := do(t, srv, "GET", path, "secret", "")
		if status != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %s", path, status, want)
		}
		for _, action := range []string{"/sync\"", "/rollback\"", "/delete-release\""} {
			if strings.Contains(body, action) {
				t.Errorf("%s offers %s", path, action)
			}
		}
	}
}

type appsetRolloutStub struct{ stub }

func (s *appsetRolloutStub) ApplicationSets() []controller.ApplicationSetStatus {
	return []controller.ApplicationSetStatus{{Name: "unbound", Generated: 3, Ready: true, Applications: []string{"unbound-dns1", "unbound-dns2", "unbound-lab"},
		Rollout: &appset.Rollout{Steps: []appset.RolloutStep{{MaxUpdate: 1, Applications: []appset.RolloutApp{
			{Name: "unbound-dns1", State: controller.RolloutFailed, Message: "automatic sync failed: <boom>"},
			{Name: "unbound-dns2", State: controller.RolloutPending, Message: "waiting"}}}},
			Unselected: []string{"unbound-lab"}}}}
}

func TestApplicationSetsPageShowsRollout(t *testing.T) {
	a := newAuthn(t, "secret")
	srv := httptest.NewServer(New(&appsetRolloutStub{}, Options{Auth: a}))
	defer srv.Close()
	code, body := do(t, srv, "GET", "/api/v1/applicationsets", "secret", "")
	if code != 200 || !strings.Contains(body, `"rollout"`) || !strings.Contains(body, `"state": "Failed"`) {
		t.Fatalf("%d %s", code, body)
	}
	code, body = do(t, srv, "GET", "/applicationsets", "secret", "")
	for _, want := range []string{"halted at step 1: unbound-dns1 failed", "Step 1", "(max 1)", `<span class="pill failed">Failed</span> unbound-dns1`,
		`<span class="pill outofsync">Pending</span> unbound-dns2`, "&lt;boom&gt;", "Not in any step", "/apps/unbound-lab"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%d missing %s: %s", code, want, body)
		}
	}
}

func TestWebHostHousekeeping(t *testing.T) {
	a := newAuthn(t, "secret")
	if err := a.AddUser("guest", auth.RoleReadonly, "guest password!"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(&stub{}, Options{Auth: a}))
	defer srv.Close()
	admin := newBrowser(t, srv)
	admin.login("", "secret")
	code, _, body := admin.do("GET", "/hosts/nas", "", nil)
	for _, want := range []string{"nas is running out of disk", "the disk is 87% full (warning at 85%)", "87% used", "13B free of 100B",
		"<td>images</td>", "2.0GB", `hx-post="/ui/hosts/nas/prune"`, `value="builder-prune"`, `<a href="/hosts/nas">nas</a> · image-prune`,
		"Total reclaimed space: 1.2GB"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("host page lacks %q: %d", want, code)
		}
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded", auth.CSRFHeader: admin.csrf, "HX-Request": "true"}
	code, _, body = admin.do("POST", "/ui/hosts/nas/prune", "kind=image-prune", form)
	if code != 200 || !strings.Contains(body, "<strong>Image prune #10 running</strong>") || !strings.Contains(body, `sse-connect="/api/v1/hosts/nas/operations/10/log"`) {
		t.Errorf("prune: %d %s", code, body)
	}
	if code, _, _ := admin.do("POST", "/ui/hosts/nas/prune", "kind=volume-prune", form); code != 400 {
		t.Errorf("bad kind: %d", code)
	}

	guest := newBrowser(t, srv)
	guest.login("guest", "guest password!")
	if code, _, body := guest.do("GET", "/hosts/nas", "", nil); code != 200 || strings.Contains(body, "Prune images") || !strings.Contains(body, "87% used") {
		t.Errorf("readonly host page: %d", code)
	}
	if code, _, _ := guest.do("POST", "/ui/hosts/nas/prune", "kind=image-prune", map[string]string{"Content-Type": "application/x-www-form-urlencoded",
		auth.CSRFHeader: guest.csrf, "HX-Request": "true"}); code != 403 {
		t.Errorf("readonly prune: %d", code)
	}

	// A host operation's output follows under the host, not an app.
	for path, want := range map[string]int{
		"/api/v1/hosts/nas/operations/10/log": 200, "/api/v1/hosts/pi/operations/10/log": 404,
		"/api/v1/apps/media/operations/10/log": 404, "/api/v1/hosts/nas/operations/7/log": 404,
	} {
		if code, body := do(t, srv, "GET", path, "secret", ""); code != want || (want == 200 && !strings.Contains(body, "Total reclaimed space")) {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}
}
