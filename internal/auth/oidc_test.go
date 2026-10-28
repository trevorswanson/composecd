package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/trevorswanson/composecd/internal/config"
)

// fakeProvider is a minimal OpenID Connect provider: discovery, JWKS, and a
// token endpoint that checks PKCE and issues ID tokens with the claims set.
type fakeProvider struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]string // code -> code_challenge
	nonce  string
	claims map[string]interface{}
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{key: key, codes: map[string]string{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		challenge, ok := f.codes[r.PostFormValue("code")]
		delete(f.codes, r.PostFormValue("code"))
		claims := map[string]interface{}{}
		for k, v := range f.claims {
			claims[k] = v
		}
		nonce := f.nonce
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		now := time.Now()
		claims["iss"], claims["aud"], claims["sub"] = f.srv.URL, "composecd", "user-1"
		claims["iat"], claims["exp"] = now.Unix(), now.Add(time.Hour).Unix()
		if _, set := claims["nonce"]; !set {
			claims["nonce"] = nonce
		}
		idt, _ := jwt.Signed(signer).Claims(claims).Serialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "at", "token_type": "Bearer", "id_token": idt, "expires_in": 3600})
	})
	return f
}

// authorize plays the provider's sign-in page: it reads the redirect composecd
// sent the browser to and returns the callback URL the provider redirects back to.
func (f *fakeProvider) authorize(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "composecd" {
		t.Fatalf("authorize request = %v", q)
	}
	f.mu.Lock()
	f.codes["code-1"] = q.Get("code_challenge")
	f.nonce = q.Get("nonce")
	f.mu.Unlock()
	return q.Get("redirect_uri") + "?code=code-1&state=" + url.QueryEscape(q.Get("state"))
}

func TestOIDC(t *testing.T) {
	f := newFakeProvider(t)
	o, err := NewOIDC(config.OIDC{
		Issuer: f.srv.URL, ClientID: "composecd", RedirectURL: "https://cd.lan/auth/callback",
		AdminGroups: []string{"admins"}, ReadonlyGroups: []string{"family"},
	})
	if err != nil {
		t.Fatal(err)
	}
	o.HTTPClient = f.srv.Client()
	if o.CallbackPath() != "/auth/callback" {
		t.Errorf("callback path = %q", o.CallbackPath())
	}

	signIn := func(claims map[string]interface{}, tamper func(*http.Request)) (*Principal, error) {
		t.Helper()
		f.mu.Lock()
		f.claims = claims
		f.mu.Unlock()
		w := httptest.NewRecorder()
		if err := o.Begin(w, httptest.NewRequest("GET", "/auth/oidc/login", nil)); err != nil {
			t.Fatal(err)
		}
		res := w.Result()
		if res.StatusCode != http.StatusFound {
			t.Fatalf("begin: %d", res.StatusCode)
		}
		cb := httptest.NewRequest("GET", f.authorize(t, res.Header.Get("Location")), nil)
		for _, c := range res.Cookies() {
			cb.AddCookie(c)
		}
		if tamper != nil {
			tamper(cb)
		}
		return o.Finish(httptest.NewRecorder(), cb)
	}

	p, err := signIn(map[string]interface{}{"preferred_username": "trevor", "groups": []string{"admins", "family"}}, nil)
	if err != nil || p.Name != "trevor" || p.Role != RoleAdmin || p.Via != ViaOIDC {
		t.Errorf("admin = %+v, %v", p, err)
	}
	p, err = signIn(map[string]interface{}{"email": "kid@home.lan", "groups": []string{"family"}}, nil)
	if err != nil || p.Name != "kid@home.lan" || p.Role != RoleReadonly {
		t.Errorf("readonly = %+v, %v", p, err)
	}
	if _, err := signIn(map[string]interface{}{"preferred_username": "guest", "groups": []string{"other"}}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("not in a group: %v", err)
	}
	if _, err := signIn(map[string]interface{}{"preferred_username": "x", "groups": []string{"admins"}, "nonce": "forged"}, nil); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("wrong nonce: %v", err)
	}
	// Without the state cookie (another browser), the callback is refused.
	_, err = signIn(map[string]interface{}{"groups": []string{"admins"}}, func(r *http.Request) { r.Header.Del("Cookie") })
	if err == nil || !strings.Contains(err.Error(), "didn't start in this browser") {
		t.Errorf("missing state cookie: %v", err)
	}
	// A provider error is reported.
	r := httptest.NewRequest("GET", "/auth/callback?error=access_denied&error_description=nope", nil)
	if _, err := o.Finish(httptest.NewRecorder(), r); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("provider error: %v", err)
	}
}

func TestOIDCRoleMapping(t *testing.T) {
	o := &OIDC{cfg: config.OIDC{AdminGroups: []string{"admins"}, GroupsClaim: "roles"}}
	if r, ok := o.role(map[string]interface{}{"roles": "users, admins"}); !ok || r != RoleAdmin {
		t.Errorf("string claim: %v %v", r, ok)
	}
	// With no readonlyGroups, anyone else may sign in read-only.
	if r, ok := o.role(map[string]interface{}{}); !ok || r != RoleReadonly {
		t.Errorf("no groups: %v %v", r, ok)
	}
	o.cfg.UsernameClaim = "nickname"
	if n := o.username(map[string]interface{}{"nickname": "t", "email": "e"}, "sub"); n != "t" {
		t.Errorf("username = %q", n)
	}
	if n := o.username(map[string]interface{}{}, "sub"); n != "sub" {
		t.Errorf("fallback username = %q", n)
	}
}
