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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/health"
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
	FollowOperation(context.Context, int64, io.Writer) error
	HostStatuses() []controller.HostStatus
	Unmanaged(context.Context, string) (*controller.Unmanaged, error)
	Adopt(context.Context, string, string) (*controller.Adoption, error)
	Root() controller.RootStatus
	Refresh()
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

	inventoryMu sync.Mutex
	inventory   map[string]*inventory
	// scanned is closed, and replaced, whenever a discovery scan starts or finishes.
	scanned chan struct{}
}

type inventory struct {
	at  time.Time
	out *controller.Unmanaged
	err error
	// running is closed when the scan in progress finishes; nil when idle.
	running chan struct{}
}

// Inventory is a host's cached discovery as the templates show it.
type Inventory struct {
	*controller.Unmanaged
	Error    string
	At       time.Time
	Scanning bool
}

// inventoryTTL is how old discovery may get before a view rescans it in the
// background. Discovery lists releases and runs compose ls and ps on the host,
// slow over SSH and too much to repeat for each render of each open tab.
const inventoryTTL = time.Minute

// inventoryWait is how long a page load waits on a host with nothing cached
// before rendering it as scanning; the scan's result arrives over SSE.
var inventoryWait = 2 * time.Second

// liveKey marks a render for an SSE update rather than a page load.
type liveKey struct{}

// inventories returns each host's cached discovery, starting a background scan
// for any that is missing or stale (or every one, with force). A page load
// waits briefly on hosts with nothing cached so quick hosts render complete.
func (s *Server) inventories(hosts []string, force, wait bool) map[string]Inventory {
	s.inventoryMu.Lock()
	if s.inventory == nil {
		s.inventory = map[string]*inventory{}
	}
	var pending []chan struct{}
	for _, host := range hosts {
		inv := s.inventory[host]
		if inv == nil {
			inv = &inventory{}
			s.inventory[host] = inv
		}
		if inv.running == nil && (force || time.Since(inv.at) >= inventoryTTL) {
			inv.running = make(chan struct{})
			go s.scan(host, inv.running)
			s.wakeLocked()
		}
		if wait && inv.at.IsZero() && inv.running != nil {
			pending = append(pending, inv.running)
		}
	}
	s.inventoryMu.Unlock()
	deadline := time.NewTimer(inventoryWait)
	defer deadline.Stop()
wait:
	for _, done := range pending {
		select {
		case <-done:
		case <-deadline.C:
			break wait
		}
	}
	out := map[string]Inventory{}
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	for _, host := range hosts {
		inv := s.inventory[host]
		view := Inventory{Unmanaged: inv.out, At: inv.at, Scanning: inv.running != nil}
		if inv.err != nil {
			view.Error = inv.err.Error()
		}
		out[host] = view
	}
	return out
}

// scan runs discovery apart from any request, so closing the page that
// started it doesn't cancel it, and wakes live views when it finishes.
func (s *Server) scan(host string, done chan struct{}) {
	out, err := s.C.Unmanaged(context.Background(), host)
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	s.inventory[host] = &inventory{at: time.Now(), out: out, err: err}
	close(done)
	s.wakeLocked()
}

// wakeLocked tells live host views that a scan started or finished.
func (s *Server) wakeLocked() {
	if s.scanned != nil {
		close(s.scanned)
	}
	s.scanned = make(chan struct{})
}

// scans returns a channel closed when a discovery scan next starts or finishes.
func (s *Server) scans() <-chan struct{} {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	if s.scanned == nil {
		s.scanned = make(chan struct{})
	}
	return s.scanned
}

type Data struct {
	User                                                       *auth.Principal
	Auth, OIDC, Admin, OOB                                     bool
	Version, Page, Tab, Name, Error, Message, Token, EventsURL string
	Root                                                       controller.RootStatus
	Apps                                                       []controller.AppStatus
	ApplicationSets                                            []controller.ApplicationSetStatus
	App                                                        controller.AppStatus
	Host                                                       controller.HostStatus
	Hosts                                                      []controller.HostStatus
	Windows                                                    []windows.Status
	Operations                                                 []*store.Operation
	History                                                    []controller.Release
	Users                                                      []*store.User
	Tokens                                                     []*store.Token
	Inventory                                                  map[string]Inventory
	Adoption                                                   *controller.Adoption
	// Op is the operation an action started, whose output the result follows.
	Op *store.Operation
	// Object is the object a dialog shows, in View.
	Object  *controller.Object
	View    string
	Filters url.Values
}

var templates = template.Must(template.New("web").Funcs(template.FuncMap{
	"lower": func(v interface{}) string { return strings.ToLower(fmt.Sprint(v)) },
	// short is a commit hash's first 7 characters, as Git abbreviates it.
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
	"shortVersion": shortVersion,
	"lines":        func(s string) []string { return strings.Split(s, "\n") },
	"lineClass": func(s string) string {
		switch {
		case strings.HasPrefix(s, "@@"):
			return "hunk"
		case strings.HasPrefix(s, "+"):
			return "added"
		case strings.HasPrefix(s, "-"):
			return "removed"
		}
		return ""
	},
	"diffstat":  diffstat,
	"graphData": graphData,
	// objectCount counts objects in sync state.
	"objectCount": func(objs []controller.Object, state string) int {
		n := 0
		for _, o := range objs {
			if string(o.Sync) == state {
				n++
			}
		}
		return n
	},
	"objectViews": objectViews,
	// children are the containers of the service id.
	"children": func(objs []controller.Object, id string) []controller.Object {
		var out []controller.Object
		for _, o := range objs {
			if o.Parent == id {
				out = append(out, o)
			}
		}
		return out
	},
	"existingAction": controller.ExistingAction,
	"tally":          tally,
	"ago":            ago,
	"iso": func(v interface{}) string {
		if t := toTime(v); !t.IsZero() {
			return t.Format(time.RFC3339)
		}
		return ""
	},
	"initial": func(s string) string {
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	// query is the filter query string with key set to v (or removed when v is "").
	"query": func(q url.Values, key, v string) string {
		out := url.Values{}
		for k, vs := range q {
			out[k] = vs
		}
		if v == "" {
			out.Del(key)
		} else {
			out.Set(key, v)
		}
		if len(out) == 0 {
			return ""
		}
		return "?" + out.Encode()
	},
	"path": url.PathEscape,
	// live reports whether the page takes SSE updates.
	"live": func(d *Data) bool { return d.Page != "settings" && d.Page != "adopt" },
	"pair": func(a, b interface{}) []interface{} { return []interface{}{a, b} },
	"list": func(v ...interface{}) []interface{} { return v },
	// busy is what a button says while an operation of kind runs.
	"busy": func(kind string) string {
		switch kind {
		case store.KindRollback:
			return "Rolling back…"
		case store.KindHeal:
			return "Healing…"
		case store.KindPrune:
			return "Pruning…"
		}
		return "Syncing…"
	},
	// state is a Compose project status's state, "running" in "running(3)".
	"state": func(s string) string {
		state, _, _ := strings.Cut(s, "(")
		return strings.ToLower(state)
	},
	// repoWeb is a repository URL a browser can open, or "" for SSH remotes.
	"repoWeb": func(u string) string {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return ""
		}
		return strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	},
	"headers": func(p *auth.Principal) string {
		b, _ := json.Marshal(map[string]string{auth.CSRFHeader: p.CSRF})
		return string(b)
	},
}).ParseFS(files, "templates/*.html"))

// Stat counts the lines a unified diff adds and removes.
type Stat struct{ Add, Del int }

func diffstat(diff string) Stat {
	var s Stat
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+"):
			s.Add++
		case strings.HasPrefix(l, "-"):
			s.Del++
		}
	}
	return s
}

// Tally counts the states worth calling out above the applications table.
type Tally struct{ Total, OutOfSync, Degraded, Progressing, Missing int }

func tally(apps []controller.AppStatus) Tally {
	t := Tally{Total: len(apps)}
	for _, a := range apps {
		if a.Sync.Status == controller.OutOfSync {
			t.OutOfSync++
		}
		switch a.Health.Status {
		case health.Degraded:
			t.Degraded++
		case health.Progressing:
			t.Progressing++
		case health.Missing:
			t.Missing++
		}
	}
	return t
}

// shortVersion cuts a full commit hash at the end of a build version
// ("dev-6e49ccac46…" becomes "dev-6e49cca") so it fits beside the brand.
func shortVersion(v string) string {
	i := strings.LastIndexAny(v, "-+.")
	tail := v[i+1:]
	if len(tail) <= 12 || strings.Trim(tail, "0123456789abcdef") != "" {
		return v
	}
	return v[:i+1] + tail[:7]
}

// ago renders t relative to now ("3 min ago", "in 2 h"), falling back to a
// date past a week. Live updates re-render it, so it stays current.
func ago(v interface{}) string {
	t := toTime(v)
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	suffix, prefix := " ago", ""
	if d < 0 {
		d, suffix, prefix = -d, "", "in "
	}
	switch {
	case d < 10*time.Second && suffix != "":
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%s%d s%s", prefix, int(d.Seconds()), suffix)
	case d < time.Hour:
		return fmt.Sprintf("%s%d min%s", prefix, int(d.Minutes()), suffix)
	case d < 24*time.Hour:
		return fmt.Sprintf("%s%d h%s", prefix, int(d.Hours()), suffix)
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%s%d d%s", prefix, int(d.Hours()/24), suffix)
	}
	return t.Local().Format("Jan 2, 2006 15:04")
}

// toTime accepts the time.Time and optional *time.Time fields templates pass.
func toTime(v interface{}) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case *time.Time:
		if x != nil {
			return *x
		}
	}
	return time.Time{}
}

func (s *Server) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	for _, route := range []string{"/{$}", "/apps", "/apps/{name}", "/hosts", "/hosts/{name}", "/windows", "/operations", "/settings", "/applicationsets"} {
		mux.HandleFunc("GET "+route, s.Page)
	}
	mux.HandleFunc("GET /hosts/{name}/adopt/{release}", s.Adopt)
	mux.HandleFunc("POST /ui/apps/{name}/{action}", s.Action)
	mux.HandleFunc("POST /ui/hosts/{name}/{action}", s.Action)
	mux.HandleFunc("POST /ui/root/{action}", s.Action)
	mux.HandleFunc("POST /ui/settings/{action}", s.Action)
	mux.HandleFunc("GET /events", s.Events)
	mux.HandleFunc("GET /api/v1/apps/{name}/containers/{container}/logs", s.Logs)
	mux.HandleFunc("GET /api/v1/apps/{name}/operations/{id}/log", s.OperationLog)
	mux.HandleFunc("GET /ui/apps/{name}/details", s.Dialog)
	mux.HandleFunc("GET /ui/apps/{name}/operations/{id}", s.Dialog)
	mux.HandleFunc("GET /ui/apps/{name}/objects/{section}/{object}", s.Dialog)
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
	switch {
	case d.Name != "" && strings.HasPrefix(r.URL.Path, "/hosts/"):
		d.Page = "host"
	case d.Name != "":
		d.Page = "detail"
	}
	if d.Page == "" {
		d.Page = "apps"
	}
	d.Tab = r.URL.Query().Get("tab")
	if d.Tab == "" || d.Tab == "summary" {
		d.Tab = "graph"
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
		case "graph", "containers", "diff":
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
	case "hosts", "host":
		live := r.Context().Value(liveKey{}) != nil
		var scan []string
		for _, h := range s.C.HostStatuses() {
			if d.Page == "host" && h.Name != d.Name {
				continue
			}
			d.Host = h
			d.Hosts = append(d.Hosts, h)
			if !h.Retired {
				scan = append(scan, h.Name)
			}
		}
		if d.Page == "host" && len(d.Hosts) == 0 {
			return nil, fmt.Errorf("host %q: %w", d.Name, controller.ErrNotFound)
		}
		d.Inventory = s.inventories(scan, false, !live)
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

func (s *Server) hostStatus(name string) (controller.HostStatus, bool) {
	for _, h := range s.C.HostStatuses() {
		if h.Name == name {
			return h, true
		}
	}
	return controller.HostStatus{}, false
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
	case errors.Is(err, controller.ErrBusy), errors.Is(err, controller.ErrBlocked), errors.Is(err, controller.ErrAdoptionBlocked), errors.Is(err, store.ErrExists):
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
	var op *store.Operation
	switch {
	case strings.HasPrefix(r.URL.Path, "/ui/apps/"):
		switch action {
		case "sync":
			op, err = s.C.Sync(name, p.Name)
		case "rollback":
			var rev int
			rev, err = strconv.Atoi(r.FormValue("revision"))
			if err == nil && rev > 0 {
				op, err = s.C.Rollback(name, rev, p.Name)
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
		target = "/hosts/" + url.PathEscape(name)
		switch action {
		case "maintenance":
			target = "/hosts"
			err = s.C.SetMaintenance(name, r.FormValue("enabled") == "true", "maintenance (by "+p.Name+")")
		case "rescan":
			if h, ok := s.hostStatus(name); !ok || h.Retired {
				err = fmt.Errorf("host %q: %w", name, controller.ErrNotFound)
				break
			}
			s.inventories([]string{name}, true, false)
			message = "Rescan started"
		default:
			err = controller.ErrNotFound
		}
	case strings.HasPrefix(r.URL.Path, "/ui/root/"):
		target = "/apps"
		if action != "refresh" {
			err = controller.ErrNotFound
			break
		}
		s.C.Refresh()
		message = "Fetching the root repository"
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
	if op != nil {
		message = fmt.Sprintf("%s #%d started", map[string]string{"sync": "Sync", "rollback": "Rollback"}[action], op.ID)
	}
	if r.Header.Get("HX-Request") == "true" {
		render(w, 200, "result", &Data{Message: message, Token: token, Op: op})
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

// Dialog renders the body of a dialog about an app: its details, an
// operation's output, or one of its objects.
func (s *Server) Dialog(w http.ResponseWriter, r *http.Request) {
	p := s.identify(w, r, auth.RoleReadonly)
	if p == nil {
		return
	}
	d := &Data{User: p, Admin: p.Role == auth.RoleAdmin, Name: r.PathValue("name"), Page: "dialog"}
	app, err := s.C.App(d.Name)
	if err != nil {
		pageError(w, err)
		return
	}
	d.App = app
	name := "app-details"
	switch {
	case r.PathValue("id") != "":
		name = "op-output"
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			pageError(w, invalidRequest("invalid operation id"))
			return
		}
		if d.Op, err = s.C.Operation(id); err == nil && d.Op.App != d.Name {
			err = fmt.Errorf("operation %d: %w", id, controller.ErrNotFound)
		}
		if err != nil {
			pageError(w, err)
			return
		}
	case r.PathValue("section") != "":
		name = "object"
		id := r.PathValue("section") + "/" + r.PathValue("object")
		for i := range app.Objects {
			if app.Objects[i].ID == id {
				d.Object = &app.Objects[i]
			}
		}
		if d.Object == nil {
			pageError(w, fmt.Errorf("object %q: %w", id, controller.ErrNotFound))
			return
		}
		d.View = r.URL.Query().Get("view")
		if !slices.Contains(objectViews(*d.Object), d.View) {
			d.View = "summary"
		}
	}
	render(w, 200, name, d)
}

// objectViews are the views an object's dialog offers.
func objectViews(o controller.Object) []string {
	views := []string{"summary"}
	if o.Kind != controller.KindContainer && o.ID != "networks/default" {
		views = append(views, "desired", "deployed")
	}
	return views
}

// graphNode is an object as graph.js draws it, without its YAML.
type graphNode struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Sub       string   `json:"sub,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	Sync      string   `json:"sync"`
	Health    string   `json:"health,omitempty"`
	Message   string   `json:"message,omitempty"`
	Uses      []string `json:"uses,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// graphData is what graph.js lays out: the app's objects and the URL their
// dialogs start with.
func graphData(a controller.AppStatus) interface{} {
	nodes := make([]graphNode, 0, len(a.Objects))
	for _, o := range a.Objects {
		n := graphNode{ID: o.ID, Kind: o.Kind, Name: o.Name, Sub: o.Image, Parent: o.Parent, Sync: string(o.Sync),
			Health: string(o.Health), Message: o.Message, Uses: o.Uses, DependsOn: o.DependsOn}
		if n.Sub == "" && o.Kind != controller.KindContainer && o.LiveName != "" {
			n.Sub = o.LiveName
		}
		nodes = append(nodes, n)
	}
	return map[string]interface{}{"app": a.Name, "base": "/ui/apps/" + url.PathEscape(a.Name) + "/objects/", "nodes": nodes}
}

// invalidRequest identifies client input errors separately from server failures.
type invalidRequest string

func (e invalidRequest) Error() string { return string(e) }

// Adopt previews a sensitive YAML export; generating it changes no state.
func (s *Server) Adopt(w http.ResponseWriter, r *http.Request) {
	p := s.identify(w, r, auth.RoleAdmin)
	if p == nil {
		return
	}
	adoption, err := s.C.Adopt(r.Context(), r.PathValue("name"), r.PathValue("release"))
	if err != nil {
		pageError(w, err)
		return
	}
	render(w, 200, "page", &Data{User: p, Admin: true, Page: "adopt", Name: r.PathValue("name"), Root: s.C.Root(), Version: s.Version, Adoption: adoption})
}
