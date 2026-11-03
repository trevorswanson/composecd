package api

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/controller"
)

// lastPlan is the request of the stub's last plan.
func lastPlan(t *testing.T, st *stub) controller.EditRequest {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.calls) - 1; i >= 0; i-- {
		if body, ok := strings.CutPrefix(st.calls[i], "plan "); ok {
			var req controller.EditRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			return req
		}
	}
	t.Fatal("nothing planned")
	return controller.EditRequest{}
}

func TestWebEditsReviewThenCommit(t *testing.T) {
	a := newAuthn(t, "secret")
	st := &webStub{}
	srv := httptest.NewServer(New(st, Options{Auth: a}))
	defer srv.Close()
	b := newBrowser(t, srv)
	b.login("", "secret")
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	// Edit mode is a form with each setting's value as it was; it takes no
	// live updates, which would wipe what is typed.
	code, _, body := b.do("GET", "/hosts/nas?edit=1", "", nil)
	for _, want := range []string{`<form id="edit-form" class="edit-form" method="post" action="/ui/edit/plan">`, `name="v.spec.url"`,
		`name="o.spec.url" value="ssh://deploy@nas"`, `<option value="local">local</option>`, `name="field" value="spec.housekeeping.diskWarning.percent"`, "Review changes"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("edit mode: %d, missing %q", code, want)
		}
	}
	if strings.Contains(body, `sse-connect="/events`) {
		t.Error("edit mode takes live updates")
	}
	if _, _, body = b.do("GET", "/hosts/nas", "", nil); !strings.Contains(body, `href="?edit=1"`) || !strings.Contains(body, `href="/edit/host/nas"`) {
		t.Error("no edit buttons")
	}
	// A generated app is edited through its ApplicationSet.
	if _, _, body = b.do("GET", "/apps/media?tab=configuration&edit=1", "", nil); strings.Contains(body, "edit-form") || !strings.Contains(body, `href="/applicationsets/stacks?edit=1"`) {
		t.Error("a generated app is editable")
	}
	if _, _, body = b.do("GET", "/applicationsets/stacks?edit=1", "", nil); !strings.Contains(body, `name="v.spec.template.spec.source.path"`) {
		t.Error("the template isn't editable")
	}

	// Only what changed is planned; an emptied setting is removed.
	values := url.Values{"csrf": {b.csrf}, "kind": {"Host"}, "name": {"nas"},
		"field":      {"spec.url", "spec.storage", "spec.env", "spec.projectRoot"},
		"o.spec.url": {"ssh://deploy@nas"}, "v.spec.url": {"ssh://deploy@nas2"},
		"o.spec.storage": {""}, "v.spec.storage": {"local"},
		"o.spec.env": {"API_TOKEN: env-secret"}, "v.spec.env": {"API_TOKEN: env-secret\r\n"},
		"o.spec.projectRoot": {"/srv"}, "v.spec.projectRoot": {""}}
	code, _, body = b.do("POST", "/ui/edit/plan", values.Encode(), form)
	req := lastPlan(t, &st.stub)
	if code != 200 || req.Set["spec.url"] != "ssh://deploy@nas2" || req.Set["spec.storage"] != "local" || len(req.Set) != 2 ||
		len(req.Unset) != 1 || req.Unset[0] != "spec.projectRoot" {
		t.Fatalf("plan: %d %+v", code, req)
	}
	for _, want := range []string{"Review the commit", "Commit to main", `name="token" value="tok"`, "url: ssh://deploy@nas2", "composecd: change spec.url of Host nas",
		"Composecd-Edited-By: admin"} {
		if !strings.Contains(body, want) {
			t.Errorf("review: missing %q", want)
		}
	}

	// Turning a section off drops changes to its settings.
	values = url.Values{"csrf": {b.csrf}, "kind": {"Application"}, "name": {"web"},
		"field":                       {"spec.syncPolicy.automated", "spec.syncPolicy.automated.selfHeal"},
		"o.spec.syncPolicy.automated": {"on"}, "o.spec.syncPolicy.automated.selfHeal": {"true"}, "v.spec.syncPolicy.automated.selfHeal": {"false"}}
	b.do("POST", "/ui/edit/plan", values.Encode(), form)
	if req = lastPlan(t, &st.stub); len(req.Set) != 0 || len(req.Unset) != 1 || req.Unset[0] != "spec.syncPolicy.automated" {
		t.Errorf("section off: %+v", req)
	}
	// What isn't a setting, or isn't one of its options, is refused.
	for field, v := range map[string]string{"metadata.name": "x", "spec.storage": "tape"} {
		values = url.Values{"csrf": {b.csrf}, "kind": {"Host"}, "name": {"nas"}, "field": {field}, "v." + field: {v}}
		if code, _, _ = b.do("POST", "/ui/edit/plan", values.Encode(), form); code != 400 {
			t.Errorf("%s=%s: %d", field, v, code)
		}
	}
	values = url.Values{"csrf": {b.csrf}, "kind": {"Host"}, "name": {"nas"}}
	if code, _, body = b.do("POST", "/ui/edit/plan", values.Encode(), form); code != 409 || !strings.Contains(body, "changes nothing") {
		t.Errorf("no change: %d", code)
	}

	// Committing needs the reviewed plan's token; a stale one shows the edit
	// planned again.
	request := `{"kind":"Host","name":"nas","set":{"spec.url":"ssh://deploy@nas2"}}`
	commit := url.Values{"csrf": {b.csrf}, "request": {request}, "token": {"old"}}
	if code, _, body = b.do("POST", "/ui/edit/commit", commit.Encode(), form); code != 409 || !strings.Contains(body, "review it again") || !strings.Contains(body, `name="token" value="tok"`) {
		t.Errorf("stale: %d", code)
	}
	commit.Set("token", "tok")
	if code, _, body = b.do("POST", "/ui/edit/commit", commit.Encode(), form); code != 200 || !strings.Contains(body, "Committed <code") || !strings.Contains(body, `href="/hosts/nas"`) {
		t.Errorf("commit: %d %s", code, body)
	}
	if st.calls[len(st.calls)-1] != "commit by admin" {
		t.Errorf("calls: %v", st.calls)
	}
	commit.Del("csrf")
	if code, _, _ = b.do("POST", "/ui/edit/commit", commit.Encode(), form); code != 403 {
		t.Errorf("CSRF: %d", code)
	}

	// The YAML editor, for an object and a new one.
	if code, _, body = b.do("GET", "/edit/host/nas", "", nil); code != 200 || !strings.Contains(body, "url: ssh://deploy@nas") || !strings.Contains(body, `value="document"`) || !strings.Contains(body, "Review deletion") {
		t.Errorf("YAML editor: %d", code)
	}
	if code, _, body = b.do("GET", "/new/window", "", nil); code != 200 || !strings.Contains(body, "kind: SyncWindow") || !strings.Contains(body, `name="file" class="mono" value="syncwindows.yaml"`) {
		t.Errorf("new window: %d %s", code, body)
	}
	values = url.Values{"csrf": {b.csrf}, "mode": {"create"}, "kind": {"SyncWindow"}, "file": {"windows.yaml"}, "document": {"kind: SyncWindow\n"}}
	if b.do("POST", "/ui/edit/plan", values.Encode(), form); !lastPlan(t, &st.stub).Create {
		t.Error("create")
	}

	// Readonly users can't edit.
	reader, err := a.CreateToken("reader", auth.RoleReadonly, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, srv, "GET", "/hosts/nas?edit=1", reader, ""); code != 200 {
		t.Errorf("readonly page: %d", code)
	} else if _, body := do(t, srv, "GET", "/hosts/nas?edit=1", reader, ""); strings.Contains(body, "edit-form") || strings.Contains(body, "Edit YAML") {
		t.Error("readonly users get the edit form")
	}
	if code, _ := do(t, srv, "GET", "/edit/host/nas", reader, ""); code != 403 {
		t.Errorf("readonly YAML editor: %d", code)
	}
}

func TestAPIEdits(t *testing.T) {
	srv, st := newServer(t, Options{})
	if code, body := do(t, srv, "GET", "/api/v1/edits/Host/nas", "secret", ""); code != 200 || !strings.Contains(body, `"file": "hosts.yaml"`) {
		t.Errorf("source: %d %s", code, body)
	}
	code, body := do(t, srv, "POST", "/api/v1/edits/plan", "secret", `{"kind":"Host","name":"nas","set":{"spec.url":"ssh://deploy@nas2"}}`)
	if code != 200 || !strings.Contains(body, `"token": "tok"`) || !strings.Contains(body, `"branch": "main"`) {
		t.Fatalf("plan: %d %s", code, body)
	}
	if code, body = do(t, srv, "POST", "/api/v1/edits/plan", "secret", `{"kind":"Host","name":"nas"}`); code != 409 {
		t.Errorf("no change: %d %s", code, body)
	}
	if code, _ = do(t, srv, "POST", "/api/v1/edits", "secret", `{"request":{"kind":"Host","name":"nas","set":{"spec.url":"x"}}}`); code != 400 {
		t.Errorf("no token: %d", code)
	}
	if code, _ = do(t, srv, "POST", "/api/v1/edits", "secret", `{"request":{"kind":"Host","name":"nas","set":{"spec.url":"x"}},"token":"old"}`); code != 409 {
		t.Errorf("stale: %d", code)
	}
	if code, body = do(t, srv, "POST", "/api/v1/edits", "secret", `{"request":{"kind":"Host","name":"nas","set":{"spec.url":"x"}},"token":"tok"}`); code != 201 || !strings.Contains(body, `"commit": "bbbb`) {
		t.Errorf("commit: %d %s", code, body)
	}
	if st.calls[len(st.calls)-1] != "commit by admin" {
		t.Errorf("calls: %v", st.calls)
	}
}
