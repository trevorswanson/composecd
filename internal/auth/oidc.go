package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/trevorswanson/composecd/internal/config"
)

// OIDC signs users in with an OpenID Connect provider, using the authorization
// code flow with PKCE. The provider is discovered on first use (and again
// after a failure), so composecd can start before the provider does.
type OIDC struct {
	cfg    config.OIDC
	secret string
	// HTTPClient talks to the provider (tests use the test server's client).
	HTTPClient *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
	pending  map[string]*pending
}

// pending is a sign-in between the redirect to the provider and the callback.
type pending struct {
	nonce, verifier string
	expires         time.Time
}

const (
	stateCookie = "composecd_oidc_state"
	pendingTTL  = 10 * time.Minute
)

// NewOIDC prepares OIDC sign-in, reading the client secret file.
func NewOIDC(cfg config.OIDC) (*OIDC, error) {
	o := &OIDC{cfg: cfg, pending: map[string]*pending{}}
	if cfg.ClientSecretFile != "" {
		b, err := os.ReadFile(cfg.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("reading auth.oidc.clientSecretFile: %w", err)
		}
		o.secret = strings.TrimSpace(string(b))
	}
	return o, nil
}

// CallbackPath is the path of the redirect URL, where the provider sends users back.
func (o *OIDC) CallbackPath() string {
	u, err := url.Parse(o.cfg.RedirectURL)
	if err != nil {
		return "/auth/callback"
	}
	return u.Path
}

func (o *OIDC) context(ctx context.Context) context.Context {
	if o.HTTPClient != nil {
		return oidc.ClientContext(ctx, o.HTTPClient)
	}
	return ctx
}

func (o *OIDC) setup(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	o.mu.Lock()
	p := o.provider
	o.mu.Unlock()
	if p == nil {
		var err error
		if p, err = oidc.NewProvider(o.context(ctx), o.cfg.Issuer); err != nil {
			return nil, nil, fmt.Errorf("discovering the OIDC provider %s: %w", o.cfg.Issuer, err)
		}
		o.mu.Lock()
		o.provider = p
		o.mu.Unlock()
	}
	scopes := o.cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email", "groups"}
	}
	return p, &oauth2.Config{
		ClientID: o.cfg.ClientID, ClientSecret: o.secret, Endpoint: p.Endpoint(),
		RedirectURL: o.cfg.RedirectURL, Scopes: scopes,
	}, nil
}

// Begin redirects the browser to the provider.
func (o *OIDC) Begin(w http.ResponseWriter, r *http.Request) error {
	_, cfg, err := o.setup(r.Context())
	if err != nil {
		return err
	}
	state, err := random(16)
	if err != nil {
		return err
	}
	nonce, err := random(16)
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()
	now := time.Now()
	o.mu.Lock()
	for k, p := range o.pending {
		if now.After(p.expires) {
			delete(o.pending, k)
		}
	}
	if len(o.pending) > 1000 {
		o.mu.Unlock()
		return errors.New("too many sign-ins in progress")
	}
	o.pending[state] = &pending{nonce: nonce, verifier: verifier, expires: now.Add(pendingTTL)}
	o.mu.Unlock()
	// The cookie ties the callback to this browser, so a link with someone
	// else's state can't sign it in.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: o.CallbackPath(), HttpOnly: true, //nolint:gosec // Secure only over HTTPS
		Secure: SecureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: int(pendingTTL.Seconds())})
	http.Redirect(w, r, cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
	return nil
}

// Finish handles the provider's redirect back and returns who signed in.
func (o *OIDC) Finish(w http.ResponseWriter, r *http.Request) (*Principal, error) {
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: o.CallbackPath(), MaxAge: -1, HttpOnly: true}) //nolint:gosec // clears the cookie
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("the provider refused the sign-in: %s %s", e, q.Get("error_description"))
	}
	state := q.Get("state")
	c, err := r.Cookie(stateCookie)
	if state == "" || err != nil || c.Value != state {
		return nil, errors.New("the sign-in expired or didn't start in this browser; try again")
	}
	o.mu.Lock()
	p := o.pending[state]
	delete(o.pending, state)
	o.mu.Unlock()
	if p == nil || time.Now().After(p.expires) {
		return nil, errors.New("the sign-in expired; try again")
	}
	provider, cfg, err := o.setup(r.Context())
	if err != nil {
		return nil, err
	}
	ctx := o.context(r.Context())
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.verifier))
	if err != nil {
		return nil, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("the provider returned no ID token")
	}
	idt, err := provider.Verifier(&oidc.Config{ClientID: o.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("verifying the ID token: %w", err)
	}
	if idt.Nonce != p.nonce {
		return nil, errors.New("the ID token's nonce doesn't match")
	}
	var claims map[string]interface{}
	if err := idt.Claims(&claims); err != nil {
		return nil, err
	}
	name := o.username(claims, idt.Subject)
	role, ok := o.role(claims)
	if !ok {
		return nil, fmt.Errorf("%s isn't in any group allowed to use composecd: %w", name, ErrForbidden)
	}
	return &Principal{Name: name, Role: role, Via: ViaOIDC}, nil
}

func (o *OIDC) username(claims map[string]interface{}, sub string) string {
	keys := []string{"preferred_username", "email"}
	if o.cfg.UsernameClaim != "" {
		keys = []string{o.cfg.UsernameClaim}
	}
	for _, k := range keys {
		if s, ok := claims[k].(string); ok && s != "" {
			return s
		}
	}
	return sub
}

// role maps the groups claim to a role: admin for adminGroups; readonly for
// readonlyGroups, or for everyone else when readonlyGroups is empty.
func (o *OIDC) role(claims map[string]interface{}) (Role, bool) {
	key := o.cfg.GroupsClaim
	if key == "" {
		key = "groups"
	}
	groups := map[string]bool{}
	switch v := claims[key].(type) {
	case []interface{}:
		for _, g := range v {
			if s, ok := g.(string); ok {
				groups[s] = true
			}
		}
	case string:
		for _, g := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			groups[g] = true
		}
	}
	for _, g := range o.cfg.AdminGroups {
		if groups[g] {
			return RoleAdmin, true
		}
	}
	if len(o.cfg.ReadonlyGroups) == 0 {
		return RoleReadonly, true
	}
	for _, g := range o.cfg.ReadonlyGroups {
		if groups[g] {
			return RoleReadonly, true
		}
	}
	return "", false
}
