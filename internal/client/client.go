// Package client talks to a composecd server's REST API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

// Client calls one server.
type Client struct {
	// Server is the base URL, e.g. http://composecd.lan:8080.
	Server string
	Token  string
	HTTP   *http.Client
}

// New returns a client for server.
func New(server, token string) *Client {
	return &Client{Server: strings.TrimRight(server, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Error is an error response from the server.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Status == http.StatusUnauthorized {
		return e.Message + " (set --token or COMPOSECD_TOKEN)"
	}
	return e.Message
}

func (c *Client) do(ctx context.Context, method, path string, in, out interface{}) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = fmt.Sprintf("%s %s: %s", method, path, resp.Status)
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding %s response: %w", path, err)
	}
	return nil
}

func appPath(name string, rest ...string) string {
	return "/api/v1/apps/" + url.PathEscape(name) + strings.Join(rest, "")
}

func (c *Client) Apps(ctx context.Context) ([]controller.AppStatus, error) {
	var out []controller.AppStatus
	return out, c.do(ctx, "GET", "/api/v1/apps", nil, &out)
}

func (c *Client) App(ctx context.Context, name string) (*controller.AppStatus, error) {
	out := &controller.AppStatus{}
	return out, c.do(ctx, "GET", appPath(name), nil, out)
}

func (c *Client) History(ctx context.Context, name string) ([]controller.Release, error) {
	var out []controller.Release
	return out, c.do(ctx, "GET", appPath(name, "/history"), nil, &out)
}

func (c *Client) Operations(ctx context.Context, app string, limit int) ([]*store.Operation, error) {
	path := "/api/v1/operations"
	if app != "" {
		path = appPath(app, "/operations")
	}
	var out []*store.Operation
	return out, c.do(ctx, "GET", path+"?limit="+strconv.Itoa(limit), nil, &out)
}

func (c *Client) Operation(ctx context.Context, id int64) (*store.Operation, error) {
	out := &store.Operation{}
	return out, c.do(ctx, "GET", "/api/v1/operations/"+strconv.FormatInt(id, 10), nil, out)
}

func (c *Client) Hosts(ctx context.Context) ([]controller.HostStatus, error) {
	var out []controller.HostStatus
	return out, c.do(ctx, "GET", "/api/v1/hosts", nil, &out)
}

func (c *Client) Root(ctx context.Context) (*controller.RootStatus, error) {
	out := &controller.RootStatus{}
	return out, c.do(ctx, "GET", "/api/v1/root", nil, out)
}

func (c *Client) Sync(ctx context.Context, name string) (*store.Operation, error) {
	out := &store.Operation{}
	return out, c.do(ctx, "POST", appPath(name, "/sync"), nil, out)
}

func (c *Client) Rollback(ctx context.Context, name string, revision int) (*store.Operation, error) {
	out := &store.Operation{}
	return out, c.do(ctx, "POST", appPath(name, "/rollback"), map[string]int{"revision": revision}, out)
}

func (c *Client) Pause(ctx context.Context, name, reason string) error {
	return c.do(ctx, "POST", appPath(name, "/pause"), map[string]string{"reason": reason}, nil)
}

func (c *Client) Resume(ctx context.Context, name string) error {
	return c.do(ctx, "POST", appPath(name, "/resume"), nil, nil)
}

func (c *Client) RefreshApp(ctx context.Context, name string) error {
	return c.do(ctx, "POST", appPath(name, "/refresh"), nil, nil)
}

func (c *Client) Refresh(ctx context.Context) error {
	return c.do(ctx, "POST", "/api/v1/refresh", nil, nil)
}

func (c *Client) Windows(ctx context.Context) ([]windows.Status, error) {
	var out []windows.Status
	return out, c.do(ctx, "GET", "/api/v1/windows", nil, &out)
}

func (c *Client) SetMaintenance(ctx context.Context, host string, on bool, reason string) error {
	body := map[string]interface{}{"enabled": on, "reason": reason}
	return c.do(ctx, "POST", "/api/v1/hosts/"+url.PathEscape(host)+"/maintenance", body, nil)
}

// Wait polls operation id until it finishes or ctx is done.
func (c *Client) Wait(ctx context.Context, id int64, every time.Duration) (*store.Operation, error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		op, err := c.Operation(ctx, id)
		if err != nil {
			return nil, err
		}
		if op.Phase != store.PhaseRunning {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-t.C:
		}
	}
}

// Me is who the client's token belongs to.
func (c *Client) Me(ctx context.Context) (*auth.Principal, error) {
	out := &auth.Principal{}
	return out, c.do(ctx, "GET", "/api/v1/me", nil, out)
}

// CreateToken makes an API token and returns it.
func (c *Client) CreateToken(ctx context.Context, name, role string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, "POST", "/api/v1/tokens", map[string]string{"name": name, "role": role}, &out)
	return out.Token, err
}

func (c *Client) Tokens(ctx context.Context) ([]*store.Token, error) {
	var out []*store.Token
	return out, c.do(ctx, "GET", "/api/v1/tokens", nil, &out)
}

func (c *Client) RevokeToken(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/api/v1/tokens/"+url.PathEscape(name), nil, nil)
}

func (c *Client) Users(ctx context.Context) ([]*store.User, error) {
	var out []*store.User
	return out, c.do(ctx, "GET", "/api/v1/users", nil, &out)
}

func (c *Client) AddUser(ctx context.Context, name, role, password string) error {
	return c.do(ctx, "POST", "/api/v1/users", map[string]string{"name": name, "role": role, "password": password}, nil)
}

func (c *Client) SetPassword(ctx context.Context, name, password string) error {
	return c.do(ctx, "PUT", "/api/v1/users/"+url.PathEscape(name)+"/password", map[string]string{"password": password}, nil)
}

func (c *Client) DeleteUser(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/api/v1/users/"+url.PathEscape(name), nil, nil)
}

func (c *Client) ApplicationSets(ctx context.Context) ([]controller.ApplicationSetStatus, error) {
	var out []controller.ApplicationSetStatus
	return out, c.do(ctx, "GET", "/api/v1/applicationsets", nil, &out)
}

func (c *Client) Unmanaged(ctx context.Context, host string) (*controller.Unmanaged, error) {
	out := &controller.Unmanaged{}
	return out, c.do(ctx, "GET", "/api/v1/hosts/"+url.PathEscape(host)+"/unmanaged", nil, out)
}

func (c *Client) Adopt(ctx context.Context, host, release string) (*controller.Adoption, error) {
	out := &controller.Adoption{}
	return out, c.do(ctx, "GET", "/api/v1/hosts/"+url.PathEscape(host)+"/adopt/"+url.PathEscape(release), nil, out)
}
