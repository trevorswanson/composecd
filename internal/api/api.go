// Package api serves composecd's REST API, its status page and /healthz.
//
// Every /api/v1 endpoint needs credentials (see package auth): a bearer token,
// or a browser session from signing in. Reading needs the readonly role and
// acting needs admin; session requests that change something must also carry
// the session's CSRF token. The exceptions verify themselves:
// /api/v1/webhook checks the Git server's signature, and /metrics and
// /healthz are open.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/web"
)

// Controller is what the API needs from the controller.
type Controller interface {
	web.Controller
	Refresh()
	RefreshRepos(urls []string) bool
	OperationMetrics() []controller.OperationMetric
}

// Options configure the server.
type Options struct {
	// Auth authenticates requests.
	Auth *auth.Authenticator
	// WebhookSecret enables POST /api/v1/webhook.
	WebhookSecret string
	Version       string
	Log           *slog.Logger
}

type server struct {
	c Controller
	Options
}

// handler serves one API route for an authenticated principal.
type handler func(r *http.Request, p *auth.Principal) (int, interface{}, error)

// New returns the HTTP handler.
func New(c Controller, o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &server{c: c, Options: o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	(&web.Server{C: c, Auth: o.Auth, Version: o.Version}).Register(mux)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	if o.Auth.OIDC != nil {
		mux.HandleFunc("GET /auth/oidc", s.oidcBegin)
		mux.HandleFunc("GET "+o.Auth.OIDC.CallbackPath(), s.oidcCallback)
	}
	mux.HandleFunc("POST /api/v1/webhook", s.webhook)
	mux.HandleFunc("GET /metrics", s.metrics)

	read := func(pattern string, h handler) { mux.HandleFunc(pattern, s.json(auth.RoleReadonly, h)) }
	act := func(pattern string, h handler) { mux.HandleFunc(pattern, s.json(auth.RoleAdmin, h)) }

	read("GET /api/v1/version", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		return http.StatusOK, map[string]string{"version": s.Version}, nil
	})
	read("GET /api/v1/me", func(_ *http.Request, p *auth.Principal) (int, interface{}, error) {
		return http.StatusOK, p, nil
	})
	read("GET /api/v1/root", func(*http.Request, *auth.Principal) (int, interface{}, error) { return http.StatusOK, c.Root(), nil })
	act("POST /api/v1/refresh", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		c.Refresh()
		return http.StatusAccepted, map[string]string{"status": "refresh requested"}, nil
	})
	read("GET /api/v1/hosts", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		return http.StatusOK, nonNil(c.HostStatuses()), nil
	})
	read("GET /api/v1/hosts/{name}/unmanaged", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		out, err := c.Unmanaged(r.Context(), r.PathValue("name"))
		return http.StatusOK, out, err
	})
	act("GET /api/v1/hosts/{name}/adopt/{release}", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		out, err := c.Adopt(r.Context(), r.PathValue("name"), r.PathValue("release"))
		return http.StatusOK, out, err
	})
	act("POST /api/v1/hosts/{name}/maintenance", func(r *http.Request, p *auth.Principal) (int, interface{}, error) {
		var body struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"reason"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		err := c.SetMaintenance(r.PathValue("name"), body.Enabled, by(body.Reason, "maintenance", p))
		return http.StatusOK, map[string]bool{"maintenance": body.Enabled}, err
	})
	read("GET /api/v1/windows", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		return http.StatusOK, nonNil(c.Windows()), nil
	})
	read("GET /api/v1/applicationsets", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		return http.StatusOK, nonNil(c.ApplicationSets()), nil
	})
	read("GET /api/v1/apps", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		return http.StatusOK, nonNil(c.Apps()), nil
	})
	read("GET /api/v1/apps/{name}", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		st, err := c.App(r.PathValue("name"))
		return http.StatusOK, st, err
	})
	read("GET /api/v1/apps/{name}/history", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		h, err := c.History(r.PathValue("name"))
		return http.StatusOK, h, err
	})
	read("GET /api/v1/apps/{name}/operations", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		limit, err := intParam(r, "limit")
		if err != nil {
			return 0, nil, err
		}
		ops, err := c.Operations(r.PathValue("name"), limit)
		return http.StatusOK, nonNil(ops), err
	})
	act("POST /api/v1/apps/{name}/sync", func(r *http.Request, p *auth.Principal) (int, interface{}, error) {
		op, err := c.Sync(r.PathValue("name"), p.Name)
		return http.StatusAccepted, op, err
	})
	act("POST /api/v1/apps/{name}/rollback", func(r *http.Request, p *auth.Principal) (int, interface{}, error) {
		var body struct {
			Revision int `json:"revision"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		if body.Revision < 0 {
			return 0, nil, badRequest("revision must not be negative")
		}
		op, err := c.Rollback(r.PathValue("name"), body.Revision, p.Name)
		return http.StatusAccepted, op, err
	})
	act("POST /api/v1/apps/{name}/pause", func(r *http.Request, p *auth.Principal) (int, interface{}, error) {
		var body struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		err := c.Pause(r.PathValue("name"), by(body.Reason, "paused", p))
		return http.StatusOK, map[string]string{"status": "paused"}, err
	})
	act("POST /api/v1/apps/{name}/resume", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		err := c.Resume(r.PathValue("name"))
		return http.StatusOK, map[string]string{"status": "resumed"}, err
	})
	act("POST /api/v1/apps/{name}/refresh", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		err := c.RefreshApp(r.PathValue("name"))
		return http.StatusAccepted, map[string]string{"status": "refresh requested"}, err
	})
	read("GET /api/v1/operations", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		limit, err := intParam(r, "limit")
		if err != nil {
			return 0, nil, err
		}
		ops, err := c.Operations("", limit)
		return http.StatusOK, nonNil(ops), err
	})
	read("GET /api/v1/operations/{id}", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return 0, nil, badRequest("invalid operation id")
		}
		op, err := c.Operation(id)
		return http.StatusOK, op, err
	})
	s.accountRoutes(read, act)
	return s.logRequests(mux)
}

// by adds who did something to a reason, e.g. "disk swap (by trevor)".
func by(reason, def string, p *auth.Principal) string {
	if reason == "" {
		reason = def
	}
	return fmt.Sprintf("%s (by %s)", reason, p.Name)
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...interface{}) error {
	return &apiError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

// json serves h as JSON to principals with the need role.
func (s *server) json(need auth.Role, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Auth.Identify(r)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthenticated) {
				s.Log.Error("authenticating a request", "path", r.URL.Path, "error", err)
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": auth.ErrUnauthenticated.Error()})
			return
		}
		if !p.Role.Allows(need) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("%s: this needs the %s role", auth.ErrForbidden, need)})
			return
		}
		if err := auth.CheckCSRF(r, p); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		status, body, err := h(r, p)
		if err != nil {
			var ae *apiError
			switch {
			case errors.As(err, &ae):
				status = ae.status
			case errors.As(err, new(*auth.InvalidError)):
				status = http.StatusBadRequest
			case errors.Is(err, controller.ErrNotFound), errors.Is(err, store.ErrNotFound):
				status = http.StatusNotFound
			case errors.Is(err, store.ErrExists):
				status = http.StatusConflict
			case errors.Is(err, controller.ErrBusy), errors.Is(err, controller.ErrBlocked), errors.Is(err, controller.ErrAdoptionBlocked):
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
	w.Header().Set("Cache-Control", "no-store")
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

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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
