package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/trevorswanson/composecd/internal/store"
)

func TestMain(m *testing.M) {
	bcryptCost = bcrypt.MinCost
	os.Exit(m.Run())
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newAuth(t *testing.T) (*Authenticator, *clock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Now()}
	return &Authenticator{Store: st, AdminToken: "bootstrap-admin-token", Now: c.now}, c
}

func bearer(tok string) *http.Request {
	r := httptest.NewRequest("GET", "/api/v1/apps", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func withCookie(method string, c *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, "/", nil)
	r.AddCookie(c)
	return r
}

func TestTokens(t *testing.T) {
	a, _ := newAuth(t)
	p, err := a.Identify(bearer("bootstrap-admin-token"))
	if err != nil || p.Role != RoleAdmin || p.Via != ViaAdminToken || p.Name != "admin" {
		t.Errorf("admin token = %+v, %v", p, err)
	}
	tok, err := a.CreateToken("grafana", RoleReadonly, "trevor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "ccd_") || len(tok) != 4+16+1+64 {
		t.Errorf("token = %q", tok)
	}
	if _, err := a.CreateToken("grafana", RoleAdmin, "trevor"); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := a.CreateToken("Bad Name", RoleAdmin, "x"); err == nil {
		t.Error("bad name accepted")
	}
	p, err = a.Identify(bearer(tok))
	if err != nil || p.Name != "grafana" || p.Role != RoleReadonly || p.Via != ViaToken || p.Session {
		t.Errorf("named token = %+v, %v", p, err)
	}
	if ts, _ := a.Store.Tokens(); ts[0].LastUsedAt == nil || ts[0].CreatedBy != "trevor" {
		t.Errorf("token record = %+v", ts[0])
	}
	// Always change the secret; replacing an existing trailing zero is a no-op.
	suffix := "0"
	if strings.HasSuffix(tok, suffix) {
		suffix = "1"
	}
	for _, bad := range []string{"", "nope", tok[:len(tok)-1] + suffix, "ccd_" + strings.Repeat("0", 16) + "_" + strings.Repeat("0", 64)} {
		if _, err := a.Identify(bearer(bad)); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("token %q: %v", bad, err)
		}
	}

	// A session signed in with the token ends when it's revoked.
	tp, _ := a.Login(context.Background(), "", tok, "10.0.0.1")
	cookie, err := a.StartSession(tp, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeToken("grafana"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Identify(bearer(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked token: %v", err)
	}
	if _, err := a.Identify(withCookie("GET", cookie)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("session of a revoked token: %v", err)
	}
}

func TestSessions(t *testing.T) {
	a, clk := newAuth(t)
	if err := a.AddUser("trevor", RoleAdmin, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	p, err := a.Login(context.Background(), "trevor", "correct horse battery", "10.0.0.1")
	if err != nil || p.Via != ViaPassword {
		t.Fatalf("login = %+v, %v", p, err)
	}
	cookie, err := a.StartSession(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Name != CookieName {
		t.Errorf("cookie = %+v", cookie)
	}
	got, err := a.Identify(withCookie("GET", cookie))
	if err != nil || got.Name != "trevor" || !got.Session || got.CSRF == "" || got.CSRF != p.CSRF {
		t.Fatalf("session = %+v, %v", got, err)
	}

	// CSRF: reads pass, writes need the token.
	if err := CheckCSRF(withCookie("GET", cookie), got); err != nil {
		t.Errorf("GET: %v", err)
	}
	post := withCookie("POST", cookie)
	if err := CheckCSRF(post, got); !errors.Is(err, ErrCSRF) {
		t.Errorf("POST without token: %v", err)
	}
	post.Header.Set(CSRFHeader, got.CSRF)
	if err := CheckCSRF(post, got); err != nil {
		t.Errorf("POST with token: %v", err)
	}
	form := httptest.NewRequest("POST", "/logout", strings.NewReader("csrf="+got.CSRF))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := CheckCSRF(form, got); err != nil {
		t.Errorf("form with token: %v", err)
	}
	if err := CheckCSRF(httptest.NewRequest("POST", "/", nil), &Principal{Via: ViaToken}); err != nil {
		t.Errorf("bearer requests don't need CSRF: %v", err)
	}

	// Role changes apply at once; deleting the user ends the session.
	if _, err := a.Store.User("trevor"); err != nil {
		t.Fatal(err)
	}
	_ = a.Store.DeleteUser("trevor")
	_ = a.AddUser("trevor", RoleReadonly, "correct horse battery")
	if _, err := a.Identify(withCookie("GET", cookie)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("session after the user was deleted: %v", err)
	}
	p, _ = a.Login(context.Background(), "trevor", "correct horse battery", "10.0.0.1")
	cookie, _ = a.StartSession(p, false)
	if got, _ := a.Identify(withCookie("GET", cookie)); got.Role != RoleReadonly {
		t.Errorf("role = %s", got.Role)
	}

	// Idle sessions expire.
	clk.t = clk.t.Add(8 * 24 * time.Hour)
	if _, err := a.Identify(withCookie("GET", cookie)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("idle session: %v", err)
	}

	// Logout ends the session.
	p, _ = a.Login(context.Background(), "trevor", "correct horse battery", "10.0.0.1")
	cookie, _ = a.StartSession(p, false)
	got, _ = a.Identify(withCookie("GET", cookie))
	if clear := a.Logout(got); clear.MaxAge >= 0 {
		t.Errorf("logout cookie = %+v", clear)
	}
	if _, err := a.Identify(withCookie("GET", cookie)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after logout: %v", err)
	}

	// Admin-token sessions end when composecd restarts.
	ap, _ := a.Login(context.Background(), "", "bootstrap-admin-token", "10.0.0.1")
	cookie, _ = a.StartSession(ap, false)
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Identify(withCookie("GET", cookie)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("admin-token session after restart: %v", err)
	}
}

func TestLoginAndLockout(t *testing.T) {
	a, clk := newAuth(t)
	ctx := context.Background()
	if err := a.AddUser("trevor", RoleAdmin, "short"); err == nil {
		t.Error("short password accepted")
	}
	if err := a.AddUser("admin", RoleAdmin, "long enough password"); err == nil {
		t.Error(`"admin" accepted as a user name`)
	}
	if err := a.AddUser("trevor", RoleAdmin, "long enough password"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "nobody", "long enough password", "ip"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("unknown user: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := a.Login(ctx, "trevor", "wrong password!", "ip"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := a.Login(ctx, "trevor", "long enough password", "ip"); !errors.Is(err, ErrLocked) {
		t.Errorf("after 5 failures: %v", err)
	}
	// Other addresses aren't locked out.
	if _, err := a.Login(ctx, "trevor", "long enough password", "other-ip"); err != nil {
		t.Errorf("other address: %v", err)
	}
	clk.t = clk.t.Add(61 * time.Second)
	if _, err := a.Login(ctx, "trevor", "long enough password", "ip"); err != nil {
		t.Errorf("after the lockout: %v", err)
	}

	if err := a.SetPassword("trevor", "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "trevor", "long enough password", "ip2"); err == nil {
		t.Error("old password still works")
	}
	if _, err := a.Login(ctx, "trevor", "a brand new password", "ip2"); err != nil {
		t.Errorf("new password: %v", err)
	}
}

func TestDisabledAndRoles(t *testing.T) {
	a := &Authenticator{Disabled: true}
	if p, err := a.Identify(httptest.NewRequest("GET", "/", nil)); err != nil || p.Role != RoleAdmin {
		t.Errorf("disabled = %+v, %v", p, err)
	}
	if !RoleAdmin.Allows(RoleReadonly) || !RoleAdmin.Allows(RoleAdmin) || RoleReadonly.Allows(RoleAdmin) || !RoleReadonly.Allows(RoleReadonly) {
		t.Error("role checks")
	}
	if _, err := ParseRole("owner"); err == nil {
		t.Error("unknown role accepted")
	}
	b, _ := newAuth(t)
	if _, err := b.Identify(httptest.NewRequest("GET", "/", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("no credentials: %v", err)
	}
}
