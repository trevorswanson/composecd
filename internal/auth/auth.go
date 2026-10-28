// Package auth authenticates API and browser requests and authorizes them by
// role. A request is authenticated by:
//
//   - a bearer token: the bootstrap admin token, or a named API token
//     (ccd_<id>_<secret>) stored as a hash;
//   - a session cookie, from signing in with a local user's password, an API
//     token, or OIDC. Sessions live in SQLite; only a hash of the cookie is
//     stored. Requests on a session that change something must carry the
//     session's CSRF token.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/trevorswanson/composecd/internal/store"
)

// Role is what a principal may do.
type Role string

const (
	// RoleReadonly may read everything (secrets masked).
	RoleReadonly Role = "readonly"
	// RoleAdmin may also act: sync, rollback, pause, maintenance, manage users and tokens.
	RoleAdmin Role = "admin"
)

// ParseRole checks a role name.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleReadonly, RoleAdmin:
		return Role(s), nil
	}
	return "", fmt.Errorf("unknown role %q (want admin or readonly)", s)
}

// Allows reports whether r includes need.
func (r Role) Allows(need Role) bool {
	return r == RoleAdmin || (r == RoleReadonly && need == RoleReadonly)
}

// How a principal authenticated.
const (
	ViaAdminToken = "admin-token"
	ViaToken      = "token"
	ViaPassword   = "password"
	ViaOIDC       = "oidc"
	ViaDisabled   = "disabled"
)

// Principal is who made a request.
type Principal struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	Via  string `json:"via"`
	// Session is set for cookie-authenticated requests.
	Session bool `json:"session"`
	// CSRF is the session's CSRF token.
	CSRF string `json:"-"`
	// sessionHash identifies the session for Logout.
	sessionHash string
}

// Session cookie and lifetime.
const (
	CookieName  = "composecd_session"
	CSRFHeader  = "X-CSRF-Token"
	sessionIdle = 7 * 24 * time.Hour
	sessionMax  = 30 * 24 * time.Hour
	// touchEvery limits writes for last-used times.
	touchEvery = time.Minute
)

// Errors.
var (
	ErrUnauthenticated = errors.New("missing or invalid credentials")
	ErrForbidden       = errors.New("not allowed for your role")
	ErrCSRF            = errors.New("missing or invalid CSRF token")
	ErrLocked          = errors.New("too many failed sign-ins; try again later")
)

// Authenticator checks credentials against the store.
type Authenticator struct {
	Store *store.Store
	// AdminToken is the bootstrap admin token ("" disables it).
	AdminToken string
	// Disabled lets every request through as admin.
	Disabled bool
	// OIDC, when set, enables signing in with an OpenID Connect provider.
	OIDC *OIDC
	// Now is the clock (tests set it).
	Now func() time.Time

	limiter limiter
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Init prepares the store: browser sessions signed in with the bootstrap admin
// token end at every start, so rotating that token can't leave old sessions
// alive.
func (a *Authenticator) Init() error {
	return a.Store.DeleteSessions("admin", ViaAdminToken)
}

// Identify authenticates r. It returns ErrUnauthenticated when r carries no
// valid credentials. It does not check CSRF; see CheckCSRF.
func (a *Authenticator) Identify(r *http.Request) (*Principal, error) {
	if a.Disabled {
		return &Principal{Name: "anonymous", Role: RoleAdmin, Via: ViaDisabled}, nil
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.token(strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
	}
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		return a.session(c.Value)
	}
	return nil, ErrUnauthenticated
}

// CheckCSRF requires session requests that change something to carry the
// session's CSRF token, in the X-CSRF-Token header or a "csrf" form field.
// Bearer-token requests aren't sent by browsers on their own, so they pass.
func CheckCSRF(r *http.Request, p *Principal) error {
	if !p.Session {
		return nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	got := r.Header.Get(CSRFHeader)
	if got == "" {
		got = r.PostFormValue("csrf")
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(p.CSRF)) != 1 {
		return ErrCSRF
	}
	return nil
}

// token checks a bearer (or signed-in-with) token.
func (a *Authenticator) token(tok string) (*Principal, error) {
	if a.AdminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.AdminToken)) == 1 {
		return &Principal{Name: "admin", Role: RoleAdmin, Via: ViaAdminToken}, nil
	}
	id, secret, ok := parseToken(tok)
	if !ok {
		return nil, ErrUnauthenticated
	}
	t, err := a.Store.Token(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash(secret)), []byte(t.SecretHash)) != 1 {
		return nil, ErrUnauthenticated
	}
	if now := a.now(); t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) > touchEvery {
		_ = a.Store.TouchToken(t.ID, now)
	}
	role, err := ParseRole(t.Role)
	if err != nil {
		return nil, err
	}
	return &Principal{Name: t.Name, Role: role, Via: ViaToken}, nil
}

func (a *Authenticator) session(cookie string) (*Principal, error) {
	h := hash(cookie)
	se, err := a.Store.Session(h)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	now := a.now()
	if now.Sub(se.LastSeenAt) > sessionIdle || now.Sub(se.CreatedAt) > sessionMax {
		_ = a.Store.DeleteSession(h)
		return nil, ErrUnauthenticated
	}
	role, err := ParseRole(se.Role)
	if err != nil {
		return nil, err
	}
	// A local user's role and existence are checked on every request, so
	// changes take effect at once.
	if se.Via == ViaPassword {
		u, err := a.Store.User(se.User)
		if err != nil {
			_ = a.Store.DeleteSession(h)
			return nil, ErrUnauthenticated
		}
		if role, err = ParseRole(u.Role); err != nil {
			return nil, err
		}
	}
	if now.Sub(se.LastSeenAt) > touchEvery {
		_ = a.Store.TouchSession(h, now)
	}
	return &Principal{Name: se.User, Role: role, Via: se.Via, Session: true, CSRF: se.CSRF, sessionHash: h}, nil
}

// StartSession records a session for p and returns the cookie to set.
func (a *Authenticator) StartSession(p *Principal, secure bool) (*http.Cookie, error) {
	id, err := random(32)
	if err != nil {
		return nil, err
	}
	csrf, err := random(32)
	if err != nil {
		return nil, err
	}
	now := a.now()
	// Drop expired sessions while we're here.
	_ = a.Store.DeleteSessionsBefore(now.Add(-sessionIdle), now.Add(-sessionMax))
	if err := a.Store.AddSession(&store.Session{
		IDHash: hash(id), User: p.Name, Role: string(p.Role), Via: p.Via, CSRF: csrf, CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		return nil, err
	}
	p.Session, p.CSRF, p.sessionHash = true, csrf, hash(id)
	// Lax, not Strict: the browser must send it when an OIDC provider
	// redirects back. CSRF tokens protect the requests that change things.
	return &http.Cookie{Name: CookieName, Value: id, Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure only over HTTPS, see SecureRequest
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionMax.Seconds())}, nil
}

// Logout ends p's session and returns a cookie that clears it.
func (a *Authenticator) Logout(p *Principal) *http.Cookie {
	if p != nil && p.sessionHash != "" {
		_ = a.Store.DeleteSession(p.sessionHash)
	}
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode} //nolint:gosec // clears the cookie
}

// Login checks a local user's password, or, with an empty name, an API token
// used as the password. ip is used for rate limiting.
func (a *Authenticator) Login(ctx context.Context, name, password, ip string) (*Principal, error) {
	key := strings.ToLower(name) + "|" + ip
	if !a.limiter.allowed(key, a.now()) {
		return nil, ErrLocked
	}
	p, err := a.login(name, password)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			a.limiter.failed(key, a.now())
		}
		return nil, err
	}
	a.limiter.succeeded(key)
	return p, nil
}

func (a *Authenticator) login(name, password string) (*Principal, error) {
	if name == "" {
		p, err := a.token(password)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	u, err := a.Store.User(name)
	if errors.Is(err, store.ErrNotFound) {
		// Spend the same time as a real check, so names can't be probed.
		_ = checkPassword(dummyHash, password)
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if err := checkPassword([]byte(u.PasswordHash), password); err != nil {
		return nil, ErrUnauthenticated
	}
	role, err := ParseRole(u.Role)
	if err != nil {
		return nil, err
	}
	return &Principal{Name: u.Name, Role: role, Via: ViaPassword}, nil
}

// SecureRequest reports whether r arrived over HTTPS (directly or through a
// proxy that says so), for the cookie's Secure flag.
func SecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
