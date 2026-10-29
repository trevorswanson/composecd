// Package infisical reads secrets from an Infisical server over its REST API.
// Values are held in memory only. Errors never include response bodies, which
// can echo secrets or credentials.
package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composecd/internal/config"
)

// Client authenticates with a machine identity (Universal Auth) or a static
// access token, and caches the session token until shortly before it expires.
type Client struct {
	site             string
	clientID         string
	clientSecretFile string
	tokenFile        string
	http             *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New returns a client for b, or nil when Infisical isn't configured.
func New(b *config.Infisical) *Client {
	if b == nil {
		return nil
	}
	site := b.SiteURL
	if site == "" {
		site = config.DefaultInfisicalSiteURL
	}
	return &Client{
		site:             strings.TrimRight(site, "/"),
		clientID:         b.ClientID,
		clientSecretFile: b.ClientSecretFile,
		tokenFile:        b.TokenFile,
		http:             &http.Client{Timeout: 30 * time.Second},
	}
}

// Secret is one key and its value. Folder is the subfolder it lives in,
// relative to the requested path (nil for the path itself and for imports).
type Secret struct {
	Key, Value string
	Folder     []string
}

// Secrets lists the secrets of s, ordered as the server returns them. Infisical
// imports are resolved by the server.
func (c *Client) Secrets(ctx context.Context, s *config.InfisicalSource) ([]Secret, error) {
	path := s.Path
	if path == "" {
		path = "/"
	}
	q := url.Values{
		"workspaceId":            {s.ProjectID},
		"environment":            {s.Environment},
		"secretPath":             {path},
		"recursive":              {fmt.Sprint(s.Recursive)},
		"expandSecretReferences": {"true"},
		"include_imports":        {"true"},
	}
	var out struct {
		Secrets []struct {
			SecretKey   string `json:"secretKey"`
			SecretValue string `json:"secretValue"`
			SecretPath  string `json:"secretPath"`
		} `json:"secrets"`
		Imports []struct {
			Secrets []struct {
				SecretKey   string `json:"secretKey"`
				SecretValue string `json:"secretValue"`
			} `json:"secrets"`
		} `json:"imports"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v3/secrets/raw?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	// Imported secrets come first, so the folder's own secrets override them.
	var res []Secret
	for _, imp := range out.Imports {
		for _, x := range imp.Secrets {
			res = append(res, Secret{Key: x.SecretKey, Value: x.SecretValue})
		}
	}
	for _, x := range out.Secrets {
		res = append(res, Secret{Key: x.SecretKey, Value: x.SecretValue, Folder: folder(path, x.SecretPath)})
	}
	return res, nil
}

// folder returns the segments of secretPath below base, or nil when secretPath
// is base itself, absent, or not under base.
func folder(base, secretPath string) []string {
	base, secretPath = strings.Trim(base, "/"), strings.Trim(secretPath, "/")
	if secretPath == base || secretPath == "" {
		return nil
	}
	if base != "" {
		if !strings.HasPrefix(secretPath, base+"/") {
			return nil
		}
		secretPath = secretPath[len(base)+1:]
	}
	return strings.Split(secretPath, "/")
}

// do sends an authenticated request. A 401 drops the cached session and
// retries once, since the token may have been revoked or expired early.
func (c *Client) do(ctx context.Context, method, path string, body, out interface{}) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.session(ctx)
		if err != nil {
			return err
		}
		status, err := c.send(ctx, method, path, tok, body, out)
		if err == nil {
			return nil
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			continue
		}
		return err
	}
}

func (c *Client) send(ctx context.Context, method, path, token string, body, out interface{}) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.site+path, rd)
	if err != nil {
		return 0, errors.New("invalid Infisical request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("infisical request canceled: %w", ctx.Err())
		}
		// The url.Error text can include the query string; keep only the host.
		return 0, fmt.Errorf("cannot reach Infisical at %s", c.site)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("infisical returned %s; check the credentials, project ID, environment and path", http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return resp.StatusCode, errors.New("infisical returned an unreadable response")
	}
	return resp.StatusCode, nil
}

// session returns a valid bearer token.
func (c *Client) session(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokenFile != "" {
		// Re-read each time so a rotated token file takes effect.
		b, err := os.ReadFile(c.tokenFile)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", errors.New("cannot read infisical.tokenFile")
		}
		return strings.TrimSpace(string(b)), nil
	}
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	b, err := os.ReadFile(c.clientSecretFile)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("cannot read infisical.clientSecretFile")
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if _, err := c.send(ctx, http.MethodPost, "/api/v1/auth/universal-auth/login", "",
		map[string]string{"clientId": c.clientID, "clientSecret": strings.TrimSpace(string(b))}, &out); err != nil {
		return "", fmt.Errorf("infisical login: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("infisical login returned no token")
	}
	c.token = out.AccessToken
	// Renew a minute early; never trust a lifetime shorter than that.
	c.expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute)
	return c.token, nil
}
