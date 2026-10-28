package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/controller"
)

func (s *stub) Unmanaged(_ context.Context, host string) (*controller.Unmanaged, error) {
	if host != "nas" {
		return nil, controller.ErrNotFound
	}
	return &controller.Unmanaged{Releases: []controller.UnmanagedRelease{{Name: "external", Revision: 1, ChartRef: "oci://registry.lan/web:1"}}, Projects: []controller.UnmanagedProject{{Name: "plain", ConfigFiles: []string{"/srv/<script>/compose.yaml"}, Hint: "Convert with composetpl create"}}}, nil
}
func (s *stub) Adopt(_ context.Context, host, name string) (*controller.Adoption, error) {
	if name == "blocked" {
		return nil, controller.ErrAdoptionBlocked
	}
	if host != "nas" || name != "external" {
		return nil, controller.ErrNotFound
	}
	return &controller.Adoption{YAML: "kind: Application\npassword: plaintext-secret\nunsafe: <script>\n", Warnings: []string{"Review plaintext values"}}, nil
}

func TestAdoptionRoutesAndRoles(t *testing.T) {
	a := newAuthn(t, "secret")
	reader, err := a.CreateToken("reader", auth.RoleReadonly, "test")
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := newServer(t, Options{Auth: a})
	for _, tc := range []struct {
		path, token string
		status      int
		want        string
	}{
		{"/api/v1/hosts/nas/unmanaged", reader, 200, "external"},
		{"/api/v1/hosts/absent/unmanaged", reader, 404, "not found"},
		{"/api/v1/hosts/nas/adopt/external", reader, 403, "admin"},
		{"/api/v1/hosts/nas/adopt/external", "", 401, "missing or invalid credentials"},
		{"/api/v1/hosts/nas/adopt/external", "secret", 200, "plaintext-secret"},
		{"/api/v1/hosts/nas/adopt/absent", "secret", 404, "not found"},
		{"/api/v1/hosts/nas/adopt/blocked", "secret", 409, "adoption is blocked"},
	} {
		code, body := do(t, srv, "GET", tc.path, tc.token, "")
		if code != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s", tc.path, code, body)
		}
		if tc.status != 200 && strings.Contains(body, "plaintext-secret") {
			t.Fatal("unauthorized export")
		}
	}
	b := newBrowser(t, srv)
	b.login("", "secret")
	code, _, body := b.do("GET", "/hosts", "", nil)
	if code != 200 || !strings.Contains(body, `href="/hosts/nas/adopt/external"`) || !strings.Contains(body, "composetpl create") || strings.Contains(body, "<script>") || strings.Contains(body, "plaintext-secret") {
		t.Fatal(code, body)
	}
	if code, _, _ := b.do("GET", "/hosts/nas/adopt/blocked", "", nil); code != 409 {
		t.Fatal(code)
	}
	code, headers, body := b.do("GET", "/hosts/nas/adopt/external", "", nil)
	if code != 200 || !strings.Contains(body, "plaintext-secret") || strings.Contains(body, "<script>") || strings.Contains(body, "sse-connect=") || headers.Get("Cache-Control") != "no-store" {
		t.Fatal(code, body)
	}
	if err := a.AddUser("guest", auth.RoleReadonly, "guest password!"); err != nil {
		t.Fatal(err)
	}
	guest := newBrowser(t, srv)
	guest.login("guest", "guest password!")
	code, _, body = guest.do("GET", "/hosts", "", nil)
	if code != 200 || strings.Contains(body, ">Adopt</a>") || strings.Contains(body, "plaintext-secret") {
		t.Fatal(code, body)
	}
	if code, _, _ := guest.do("GET", "/hosts/nas/adopt/external", "", nil); code != 403 {
		t.Fatal(code)
	}
	// Authenticated inventory failures are shown per-host, without losing the page.
	failure := &discoveryFailure{stub: stub{}}
	fs := httptest.NewServer(New(failure, Options{Auth: a}))
	defer fs.Close()
	if code, body := do(t, fs, "GET", "/hosts", "secret", ""); code != 200 || !strings.Contains(body, "Discovery failed") {
		t.Fatal(code, body)
	}
}

type discoveryFailure struct{ stub }

func (*discoveryFailure) Unmanaged(context.Context, string) (*controller.Unmanaged, error) {
	return nil, controller.ErrNotFound
}
