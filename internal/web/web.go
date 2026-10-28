// Package web serves the embedded browser interface. It shares the API's
// authenticator and controller; no separate browser credentials are kept.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

//go:embed templates/*.html static/*
var files embed.FS

type Controller interface {
	Apps() []controller.AppStatus
	ApplicationSets() []controller.ApplicationSetStatus
	App(string) (controller.AppStatus, error)
	History(string) ([]controller.Release, error)
	Operations(string, int) ([]*store.Operation, error)
	Operation(int64) (*store.Operation, error)
	HostStatuses() []controller.HostStatus
	Root() controller.RootStatus
	Windows() []windows.Status
	Sync(string, string) (*store.Operation, error)
	Rollback(string, int, string) (*store.Operation, error)
	Pause(string, string) error
	Resume(string) error
	RefreshApp(string) error
	SetMaintenance(string, bool, string) error
	Subscribe() (<-chan controller.Update, func())
	Logs(context.Context, string, string, int, bool, io.Writer) error
	Mask(string, string) string
}

type Server struct {
	C       Controller
	Auth    *auth.Authenticator
	Version string
}

type Data struct {
	User                                                       *auth.Principal
	Auth, OIDC, Admin, OOB                                     bool
	Version, Page, Tab, Name, Error, Message, Token, EventsURL string
	Root                                                       controller.RootStatus
	Apps                                                       []controller.AppStatus
	ApplicationSets                                            []controller.ApplicationSetStatus
	App                                                        controller.AppStatus
	Hosts                                                      []controller.HostStatus
	Windows                                                    []windows.Status
	Operations                                                 []*store.Operation
	History                                                    []controller.Release
	Users                                                      []*store.User
	Tokens                                                     []*store.Token
	Filters                                                    url.Values
}

var templates = template.Must(template.New("web").Funcs(template.FuncMap{
	"lower": func(v interface{}) string { return strings.ToLower(fmt.Sprint(v)) },
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
	"lines": func(s string) []string { return strings.Split(s, "\n") },
	"lineClass": func(s string) string {
		if strings.HasPrefix(s, "+") {
			return "added"
		}
		if strings.HasPrefix(s, "-") {
			return "removed"
		}
		return ""
	},
	"path": url.PathEscape,
	"headers": func(p *auth.Principal) string {
		b, _ := json.Marshal(map[string]string{auth.CSRFHeader: p.CSRF})
		return string(b)
	},
}).ParseFS(files, "templates/*.html"))

func (s *Server) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	for _, route := range []string{"/{$}", "/apps", "/apps/{name}", "/hosts", "/windows", "/operations", "/settings", "/applicationsets"} {
		mux.HandleFunc("GET "+route, s.Page)
	}
	mux.HandleFunc("POST /ui/apps/{name}/{action}", s.Action)
	mux.HandleFunc("POST /ui/hosts/{name}/maintenance", s.Action)
	mux.HandleFunc("POST /ui/settings/{action}", s.Action)
	mux.HandleFunc("GET /events", s.Events)
	mux.HandleFunc("GET /api/v1/apps/{name}/containers/{container}/logs", s.Logs)
}

func (s *Server) identify(w http.ResponseWriter, r *http.Request, need auth.Role) *auth.Principal {
	p, err := s.Auth.Identify(r)
	if err != nil {
		http.Error(w, "Sign in required", http.StatusUnauthorized)
		return nil
	}
	if !p.Role.Allows(need) {
		http.Error(w, "This needs the admin role", http.StatusForbidden)
		return nil
	}
	if err = auth.CheckCSRF(r, p); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return nil
	}
	return p
}

var signInErrors = map[string]string{
	"failed":    "That user name and password (or token) aren't valid.",
	"locked":    "Too many failed sign-ins. Wait a minute and try again.",
	"oidc":      "Signing in with your identity provider didn't work. Try again, or check composecd's log.",
	"forbidden": "You signed in, but you aren't in a group allowed to use composecd.",
}

func (s *Server) data(r *http.Request, p *auth.Principal) (*Data, error) {
	d := &Data{User: p, Auth: !s.Auth.Disabled, OIDC: s.Auth.OIDC != nil, Admin: p.Role == auth.RoleAdmin, Version: s.Version, Filters: r.URL.Query(), Root: s.C.Root()}
	d.Page = strings.Trim(r.URL.Path, "/")
	d.Name = r.PathValue("name")
	if d.Name != "" {
		d.Page = "detail"
	}
	if d.Page == "" {
		d.Page = "apps"
	}
	d.Tab = r.URL.Query().Get("tab")
	if d.Tab == "" {
		d.Tab = "summary"
	}
	switch d.Page {
	case "apps":
		d.Hosts = s.C.HostStatuses()
		d.Windows = s.C.Windows()
		for _, a := range s.C.Apps() {
			if matches(a, d.Filters) {
				d.Apps = append(d.Apps, a)
			}
		}
	case "detail":
		a, err := s.C.App(d.Name)
		if err != nil {
			return nil, err
		}
		d.App = a
		switch d.Tab {
		case "summary", "containers", "diff":
		case "history":
			d.History, err = s.C.History(d.Name)
		case "operations":
			d.Operations, err = s.operations(d.Name)
		default:
			return nil, controller.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		d.App.Sync.Changes = append([]controller.Change(nil), a.Sync.Changes...)
		for i := range d.App.Sync.Changes {
			d.App.Sync.Changes[i].Diff = s.C.Mask(d.Name, d.App.Sync.Changes[i].Diff)
		}
	case "applicationsets":
		d.ApplicationSets = s.C.ApplicationSets()
	case "hosts":
		d.Hosts = s.C.HostStatuses()
	case "windows":
		d.Windows = s.C.Windows()
	case "operations":
		var err error
		d.Operations, err = s.operations("")
		if err != nil {
			return nil, err
		}
	case "settings":
		if d.Admin && s.Auth.Store != nil {
			var err error
			d.Users, err = s.Auth.Store.Users()
			if err != nil {
				return nil, err
			}
			d.Tokens, err = s.Auth.Store.Tokens()
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, controller.ErrNotFound
	}
	q := r.URL.Query()
	q.Set("page", r.URL.Path)
	d.EventsURL = "/events?" + q.Encode()
	return d, nil
}

func matches(a controller.AppStatus, q url.Values) bool {
	if v := q.Get("host"); v != "" && a.Host != v {
		return false
	}
	if v := q.Get("sync"); v != "" && string(a.Sync.Status) != v {
		return false
	}
	if v := q.Get("health"); v != "" && string(a.Health.Status) != v {
		return false
	}
	if v := q.Get("label"); v != "" {
		k, want, has := strings.Cut(v, "=")
		got, ok := a.Labels[k]
		if !ok || (has && got != want) {
			return false
		}
	}
	return true
}

func (s *Server) operations(app string) ([]*store.Operation, error) {
	ops, err := s.C.Operations(app, 50)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Operation, 0, len(ops))
	for _, op := range ops {
		full, err := s.C.Operation(op.ID)
		if err != nil {
			return nil, err
		}
		copyOp := *full
		copyOp.Log = s.C.Mask(full.App, full.Log)
		copyOp.Message = s.C.Mask(full.App, full.Message)
		out = append(out, &copyOp)
	}
	return out, nil
}

func render(w http.ResponseWriter, status int, name string, d *Data) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, d); err != nil {
		http.Error(w, "Could not render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

func (s *Server) Page(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.Identify(r)
	if err != nil {
		render(w, 401, "page", &Data{Auth: !s.Auth.Disabled, OIDC: s.Auth.OIDC != nil, Error: signInErrors[r.URL.Query().Get("error")]})
		return
	}
	d, err := s.data(r, p)
	if err != nil {
		pageError(w, err)
		return
	}
	render(w, 200, "page", d)
}

func pageError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, new(*auth.InvalidError)), errors.As(err, new(invalidRequest)):
		code = http.StatusBadRequest
	case errors.Is(err, controller.ErrNotFound), errors.Is(err, store.ErrNotFound):
		code = 404
	case errors.Is(err, controller.ErrBusy), errors.Is(err, controller.ErrBlocked), errors.Is(err, store.ErrExists):
		code = 409
	}
	http.Error(w, err.Error(), code)
}

// Action accepts ordinary forms and htmx forms through the same role and CSRF
// checks. The inherited hx-headers sends the per-session token on every action.
func (s *Server) Action(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	p := s.identify(w, r, auth.RoleAdmin)
	if p == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	name, action := r.PathValue("name"), r.PathValue("action")
	var err error
	target := "/apps/" + url.PathEscape(name)
	message, token := "Action requested", ""
	switch {
	case strings.HasPrefix(r.URL.Path, "/ui/apps/"):
		switch action {
		case "sync":
			_, err = s.C.Sync(name, p.Name)
		case "rollback":
			var rev int
			rev, err = strconv.Atoi(r.FormValue("revision"))
			if err == nil && rev > 0 {
				_, err = s.C.Rollback(name, rev, p.Name)
			} else {
				err = invalidRequest("invalid revision")
			}
		case "pause":
			err = s.C.Pause(name, "paused (by "+p.Name+")")
		case "resume":
			err = s.C.Resume(name)
		case "refresh":
			err = s.C.RefreshApp(name)
		default:
			err = controller.ErrNotFound
		}
	case strings.HasPrefix(r.URL.Path, "/ui/hosts/"):
		target = "/hosts"
		err = s.C.SetMaintenance(name, r.FormValue("enabled") == "true", "maintenance (by "+p.Name+")")
	case strings.HasPrefix(r.URL.Path, "/ui/settings/"):
		target = "/settings"
		if s.Auth.Store == nil {
			err = errors.New("account store unavailable")
			break
		}
		switch action {
		case "token-create":
			var role auth.Role
			role, err = auth.ParseRole(r.FormValue("role"))
			if err == nil {
				token, err = s.Auth.CreateToken(r.FormValue("name"), role, p.Name)
			}
		case "token-revoke":
			err = s.Auth.RevokeToken(r.FormValue("name"))
		case "user-add":
			var role auth.Role
			role, err = auth.ParseRole(r.FormValue("role"))
			if err == nil {
				err = s.Auth.AddUser(r.FormValue("name"), role, r.FormValue("password"))
			}
		case "user-password":
			err = s.Auth.SetPassword(r.FormValue("name"), r.FormValue("password"))
		case "user-delete":
			err = s.Auth.Store.DeleteUser(r.FormValue("name"))
		default:
			err = controller.ErrNotFound
		}
	}
	if err != nil {
		pageError(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		render(w, 200, "result", &Data{Message: message, Token: token})
		return
	}
	if token != "" {
		r.URL.Path = "/settings"
		r.SetPathValue("name", "")
		d, e := s.data(r, p)
		if e != nil {
			pageError(w, e)
			return
		}
		d.Token = token
		render(w, 200, "page", d)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// invalidRequest identifies client input errors separately from server failures.
type invalidRequest string

func (e invalidRequest) Error() string { return string(e) }
