// Package api serves composecd's REST API, its status page and /healthz.
//
// Every /api/v1 endpoint needs the admin token as "Authorization: Bearer
// <token>", unless authentication is disabled. The exceptions verify
// themselves: /api/v1/webhook checks the Git server's signature. /metrics and
// /healthz are open. The status page also accepts
// the token as a cookie set by its login form; the cookie is only honored for
// GET requests, so it can't be used to trigger operations.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

// Controller is what the API needs from the controller.
type Controller interface {
	Apps() []controller.AppStatus
	App(name string) (controller.AppStatus, error)
	History(name string) ([]controller.Release, error)
	Operations(app string, limit int) ([]*store.Operation, error)
	Operation(id int64) (*store.Operation, error)
	HostStatuses() []controller.HostStatus
	Root() controller.RootStatus
	Sync(name string) (*store.Operation, error)
	Rollback(name string, revision int) (*store.Operation, error)
	Pause(name, reason string) error
	Resume(name string) error
	RefreshApp(name string) error
	Refresh()
	RefreshRepos(urls []string) bool
	Windows() []windows.Status
	SetMaintenance(host string, on bool, reason string) error
	OperationMetrics() []controller.OperationMetric
}

// Options configure the server.
type Options struct {
	// Token is the admin token; ignored when AuthDisabled.
	Token        string
	AuthDisabled bool
	// WebhookSecret enables POST /api/v1/webhook.
	WebhookSecret string
	Version       string
	Log           *slog.Logger
}

// cookieName holds the token for the status page.
const cookieName = "composecd_token"

type server struct {
	c Controller
	Options
}

// New returns the HTTP handler.
func New(c Controller, o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &server{c: c, Options: o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /{$}", s.page(s.statusPage))
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("POST /api/v1/webhook", s.webhook)
	mux.HandleFunc("GET /metrics", s.metrics)

	api := func(pattern string, h func(*http.Request) (int, interface{}, error)) {
		mux.HandleFunc(pattern, s.json(h))
	}
	api("GET /api/v1/version", func(*http.Request) (int, interface{}, error) {
		return http.StatusOK, map[string]string{"version": s.Version}, nil
	})
	api("GET /api/v1/root", func(*http.Request) (int, interface{}, error) { return http.StatusOK, c.Root(), nil })
	api("POST /api/v1/refresh", func(*http.Request) (int, interface{}, error) {
		c.Refresh()
		return http.StatusAccepted, map[string]string{"status": "refresh requested"}, nil
	})
	api("GET /api/v1/hosts", func(*http.Request) (int, interface{}, error) { return http.StatusOK, nonNil(c.HostStatuses()), nil })
	api("POST /api/v1/hosts/{name}/maintenance", func(r *http.Request) (int, interface{}, error) {
		var body struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"reason"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		err := c.SetMaintenance(r.PathValue("name"), body.Enabled, body.Reason)
		return http.StatusOK, map[string]bool{"maintenance": body.Enabled}, err
	})
	api("GET /api/v1/windows", func(*http.Request) (int, interface{}, error) { return http.StatusOK, nonNil(c.Windows()), nil })
	api("GET /api/v1/apps", func(*http.Request) (int, interface{}, error) { return http.StatusOK, nonNil(c.Apps()), nil })
	api("GET /api/v1/apps/{name}", func(r *http.Request) (int, interface{}, error) {
		st, err := c.App(r.PathValue("name"))
		return http.StatusOK, st, err
	})
	api("GET /api/v1/apps/{name}/history", func(r *http.Request) (int, interface{}, error) {
		h, err := c.History(r.PathValue("name"))
		return http.StatusOK, h, err
	})
	api("GET /api/v1/apps/{name}/operations", func(r *http.Request) (int, interface{}, error) {
		limit, err := intParam(r, "limit")
		if err != nil {
			return 0, nil, err
		}
		ops, err := c.Operations(r.PathValue("name"), limit)
		return http.StatusOK, nonNil(ops), err
	})
	api("POST /api/v1/apps/{name}/sync", func(r *http.Request) (int, interface{}, error) {
		op, err := c.Sync(r.PathValue("name"))
		return http.StatusAccepted, op, err
	})
	api("POST /api/v1/apps/{name}/rollback", func(r *http.Request) (int, interface{}, error) {
		var body struct {
			Revision int `json:"revision"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		if body.Revision < 0 {
			return 0, nil, badRequest("revision must not be negative")
		}
		op, err := c.Rollback(r.PathValue("name"), body.Revision)
		return http.StatusAccepted, op, err
	})
	api("POST /api/v1/apps/{name}/pause", func(r *http.Request) (int, interface{}, error) {
		var body struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		err := c.Pause(r.PathValue("name"), body.Reason)
		return http.StatusOK, map[string]string{"status": "paused"}, err
	})
	api("POST /api/v1/apps/{name}/resume", func(r *http.Request) (int, interface{}, error) {
		err := c.Resume(r.PathValue("name"))
		return http.StatusOK, map[string]string{"status": "resumed"}, err
	})
	api("POST /api/v1/apps/{name}/refresh", func(r *http.Request) (int, interface{}, error) {
		err := c.RefreshApp(r.PathValue("name"))
		return http.StatusAccepted, map[string]string{"status": "refresh requested"}, err
	})
	api("GET /api/v1/operations", func(r *http.Request) (int, interface{}, error) {
		limit, err := intParam(r, "limit")
		if err != nil {
			return 0, nil, err
		}
		ops, err := c.Operations("", limit)
		return http.StatusOK, nonNil(ops), err
	})
	api("GET /api/v1/operations/{id}", func(r *http.Request) (int, interface{}, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return 0, nil, badRequest("invalid operation id")
		}
		op, err := c.Operation(id)
		return http.StatusOK, op, err
	})
	return s.logRequests(mux)
}

// authorized checks the bearer token, or, for GET requests, the page cookie.
func (s *server) authorized(r *http.Request) bool {
	if s.AuthDisabled {
		return true
	}
	if s.Token == "" {
		return false
	}
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(cookieName); err == nil && r.Method == http.MethodGet {
		got = c.Value
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...interface{}) error {
	return &apiError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

// json adapts a handler returning (status, body, error) to JSON over HTTP.
func (s *server) json(h func(*http.Request) (int, interface{}, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid token"})
			return
		}
		status, body, err := h(r)
		if err != nil {
			var ae *apiError
			switch {
			case errors.As(err, &ae):
				status = ae.status
			case errors.Is(err, controller.ErrNotFound):
				status = http.StatusNotFound
			case errors.Is(err, controller.ErrBusy), errors.Is(err, controller.ErrBlocked):
				status = http.StatusConflict
			default:
				status = http.StatusInternalServerError
				s.Log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
			}
			body = map[string]string{"error": err.Error()}
		}
		writeJSON(w, status, body)
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// decode reads an optional JSON body into v.
func decode(r *http.Request, v interface{}) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid request body: %v", err)
	}
	return nil
}

func intParam(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, badRequest("invalid %s %q", name, v)
	}
	return n, nil
}

// nonNil makes empty lists encode as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" {
			s.Log.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
		}
	})
}
