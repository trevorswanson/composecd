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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/features"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/images"
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
	HostResources(context.Context, string) (*hosts.Resources, error)
	Adopt(context.Context, string, string) (*controller.Adoption, error)
	Root() controller.RootStatus
	RefreshWait(context.Context) (controller.RootStatus, error)
	Windows() []windows.Status
	Sync(string, string) (*store.Operation, error)
	Rollback(string, int, string) (*store.Operation, error)
	Pause(string, string) error
	Resume(string) error
	RefreshApp(string) error
	RefreshAppWait(context.Context, string) error
	CheckImages(string) error
	CheckImagesWait(context.Context, string) (controller.ImageCheck, error)
	SetMaintenance(string, bool, string) error
	DeleteRelease(host, name, user string) (*store.Operation, error)
	PruneHost(host, kind string, all bool, user string) (*store.Operation, error)
	HostOperations(string, int) ([]*store.Operation, error)
	Subscribe() (<-chan controller.Update, func())
	Logs(context.Context, string, string, int, bool, io.Writer) error
	Mask(string, string) string
	LiveObject(ctx context.Context, app, id string, all, redact bool) (*controller.Live, error)
	HostObject(ctx context.Context, host, kind, name string, redact bool) (*controller.HostObject, error)
	DecideImage(app, service, choice, user string) (*store.ImageDecision, error)
	DecideUpgrade(app, service, choice, tag, user string) (*store.ImageUpgrade, error)
	EditSource(ctx context.Context, kind, name string) (file, document string, err error)
	PlanEdit(context.Context, controller.EditRequest) (*controller.EditPlan, error)
	ApplyEdit(ctx context.Context, req controller.EditRequest, token, user string) (*controller.EditResult, error)
	Objects() *config.Objects
	Bootstrap() *config.Bootstrap
}

type Server struct {
	C       Controller
	Auth    *auth.Authenticator
	Version string

	inventoryMu sync.Mutex
	inventory   map[string]*scan[controller.Unmanaged]
	resources   map[string]*scan[hosts.Resources]
	// scanned is closed, and replaced, whenever a host scan starts or finishes.
	scanned chan struct{}
}

// scan is a host's cached discovery or resource listing.
type scan[T any] struct {
	at  time.Time
	out *T
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
	// Graph is everything on the host, by stack, as graph.js draws it; nil
	// until its resources are listed. GraphError is why they couldn't be.
	Graph      *graph
	GraphError string
}

// inventoryTTL is how old discovery may get before a view rescans it in the
// background. Discovery lists releases and runs compose ls and ps on the host,
// slow over SSH and too much to repeat for each render of each open tab.
const inventoryTTL = time.Minute

// cached returns each host's cached result in cache, starting a background run
// for any that is missing or stale (or every one, with force). It never waits
// on a host: deciding to scan is a look at when the last run ended, and the
// lock is only held for that and to record a result, never across a run. A
// host with nothing cached renders as scanning, and live views wake when a
// run ends and, with announce, when one starts, so its result arrives over SSE.
func cached[T any](s *Server, cache *map[string]*scan[T], hostNames []string, force, announce bool, run func(string) (*T, error)) map[string]scan[T] {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	if *cache == nil {
		*cache = map[string]*scan[T]{}
	}
	out := make(map[string]scan[T], len(hostNames))
	for _, host := range hostNames {
		sc := (*cache)[host]
		if sc == nil {
			sc = &scan[T]{}
			(*cache)[host] = sc
		}
		if sc.running == nil && (force || time.Since(sc.at) >= inventoryTTL) {
			sc.running = make(chan struct{})
			// The scan runs apart from any request, so closing the page that
			// started it doesn't cancel it, and wakes live views when it ends.
			go func(host string, done chan struct{}) {
				out, err := run(host)
				s.inventoryMu.Lock()
				defer s.inventoryMu.Unlock()
				(*cache)[host] = &scan[T]{at: time.Now(), out: out, err: err}
				close(done)
				s.wakeLocked()
			}(host, sc.running)
			if announce {
				s.wakeLocked()
			}
		}
		out[host] = *sc
	}
	return out
}

// inventories returns each host's cached discovery; see cached.
func (s *Server) inventories(hostNames []string, force bool) map[string]Inventory {
	got := cached(s, &s.inventory, hostNames, force, true, func(host string) (*controller.Unmanaged, error) {
		return s.C.Unmanaged(context.Background(), host)
	})
	out := map[string]Inventory{}
	for host, sc := range got {
		view := Inventory{Unmanaged: sc.out, At: sc.at, Scanning: sc.running != nil}
		if sc.err != nil {
			view.Error = sc.err.Error()
		}
		out[host] = view
	}
	return out
}

// hostResources returns each host's cached resource listing; see cached.
func (s *Server) hostResources(hostNames []string, force bool) map[string]scan[hosts.Resources] {
	// Nothing shows a resource scan running, so only its end wakes views.
	return cached(s, &s.resources, hostNames, force, false, func(host string) (*hosts.Resources, error) {
		return s.C.HostResources(context.Background(), host)
	})
}

// wakeLocked tells live host views that a scan started or finished.
func (s *Server) wakeLocked() {
	if s.scanned != nil {
		close(s.scanned)
	}
	s.scanned = make(chan struct{})
}

// scans returns a channel closed when a host scan next starts or finishes.
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
	// Graph is the graph tab's: the app's objects and their neighbors.
	Graph    graph
	Adoption *controller.Adoption
	// Op is the operation an action started, whose output the result follows.
	Op *store.Operation
	// Toast is an action's outcome, shown briefly; Message titles a result
	// that stays, for a token or an operation's output.
	Toast string
	// Object is the object a dialog shows, in View.
	Object *controller.Object
	View   string
	// Live is Object's state on its host, for its live and diff views; All
	// includes live keys Git doesn't set (or, on the diff tab, synced objects).
	Live *controller.Live
	All  bool
	// HostObject is the engine object a host graph's dialog shows, for objects
	// no Application owns.
	HostObject *controller.HostObject
	// Mode is the diff tab's: "" compares the compose file, "objects" each
	// object's live state.
	Mode    string
	Objects []ObjectDiff
	// Files are the compose file's resources side by side; Full shows every
	// one in whole rather than only the changed lines.
	Files   []FileDiff
	Full    bool
	Filters url.Values
	// Settings are the page's object's (or the server's) settings, set or
	// not, filtered by Show: "", "set" or "unset". On an ApplicationSet's
	// page, Template are its template's.
	Settings, Template Settings
	Show               string
	// AppSet is the ApplicationSet an applicationset page shows.
	AppSet controller.ApplicationSetStatus
	// Editing shows the page's settings as a form (edit=1); Edit is the edit
	// an edit page shows.
	Editing bool
	Edit    *Edit
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
	// digest abbreviates an image digest: sha256:0123456789ab.
	"digest": images.Short,
	// imageName and imageDigest split an image reference at its digest:
	// nginx:1.27 and sha256:... for nginx:1.27@sha256:...
	"imageName":   func(s string) string { name, _, _ := strings.Cut(s, "@"); return name },
	"imageDigest": func(s string) string { _, d, _ := strings.Cut(s, "@"); return d },
	// imageUpdates counts the images with an update available.
	"imageUpdates": func(ims []controller.ImageStatus) int {
		n := 0
		for _, im := range ims {
			if im.UpdateAvailable {
				n++
			}
		}
		return n
	},
	"shortVersion": shortVersion,
	"inc":          func(i int) int { return i + 1 },
	// rolloutPill is the pill class of a rolling sync state.
	"rolloutPill": func(state string) string {
		switch state {
		case controller.RolloutDone:
			return "ready"
		case controller.RolloutUpdating, controller.RolloutStabilizing:
			return "progressing"
		case controller.RolloutPending:
			return "outofsync"
		case controller.RolloutUnhealthy:
			return "degraded"
		case controller.RolloutFailed:
			return "failed"
		}
		return "unknown"
	},
	"lines": func(s string) []string { return strings.Split(s, "\n") },
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
	"diffstat": diffstat,
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
	// bytes is a size with decimal units, as docker prints it.
	"bytes": hosts.FormatBytes,
	// usedPercent is how full a measured disk is, "87%".
	"usedPercent": func(u *hosts.DiskUsage) string {
		if r, ok := u.UsedRatio(); ok {
			return fmt.Sprintf("%.0f%%", r*100)
		}
		return ""
	},
	// live reports whether the page takes SSE updates.
	"live": func(d *Data) bool {
		return d.Page != "settings" && d.Page != "adopt" && !d.Editing && !slices.Contains(editPages, d.Page)
	},
	"editable": editable,
	// inForm reports whether an edit form edits the setting: service labels
	// are in compose files, not composecd's objects.
	"inForm":  func(s features.Setting) bool { return s.Scope != features.ServiceLabel && s.Scope != features.Server },
	"inputID": func(path string) string { return "in-" + strings.NewReplacer(".", "-", "[]", "").Replace(path) },
	"pair":    func(a, b interface{}) []interface{} { return []interface{}{a, b} },
	"list":    func(v ...interface{}) []interface{} { return v },
	// busy is what a button says while an operation of kind runs.
	"busy": func(kind string) string {
		switch kind {
		case store.KindRollback:
			return "Rolling back…"
		case store.KindHeal:
			return "Healing…"
		case store.KindPrune, store.KindImagePrune, store.KindBuilderPrune:
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
	for _, route := range []string{"/{$}", "/apps", "/apps/{name}", "/hosts", "/hosts/{name}", "/windows", "/operations", "/settings", "/applicationsets", "/applicationsets/{name}"} {
		mux.HandleFunc("GET "+route, s.Page)
	}
	mux.HandleFunc("GET /hosts/{name}/adopt/{release}", s.Adopt)
	mux.HandleFunc("GET /edit/{kind}/{name}", s.EditPage)
	mux.HandleFunc("GET /new/{kind}", s.EditPage)
	mux.HandleFunc("POST /ui/edit/plan", s.PlanEdit)
	mux.HandleFunc("POST /ui/edit/commit", s.CommitEdit)
	mux.HandleFunc("POST /ui/apps/{name}/{action}", s.Action)
	mux.HandleFunc("POST /ui/hosts/{name}/{action}", s.Action)
	mux.HandleFunc("POST /ui/root/{action}", s.Action)
	mux.HandleFunc("POST /ui/settings/{action}", s.Action)
	mux.HandleFunc("GET /events", s.Events)
	mux.HandleFunc("GET /api/v1/apps/{name}/containers/{container}/logs", s.Logs)
	mux.HandleFunc("GET /api/v1/apps/{name}/operations/{id}/log", s.OperationLog)
	mux.HandleFunc("GET /api/v1/hosts/{name}/operations/{id}/log", s.OperationLog)
	mux.HandleFunc("GET /ui/apps/{name}/details", s.Dialog)
	mux.HandleFunc("GET /ui/apps/{name}/operations/{id}", s.Dialog)
	mux.HandleFunc("GET /ui/apps/{name}/objects/{section}/{object}", s.Dialog)
	mux.HandleFunc("GET /ui/hosts/{name}/objects/{kind}/{object...}", s.HostDialog)
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
	case d.Name != "" && strings.HasPrefix(r.URL.Path, "/applicationsets/"):
		d.Page = "applicationset"
	case d.Name != "":
		d.Page = "detail"
	}
	if d.Page == "" {
		d.Page = "apps"
	}
	d.Show = r.URL.Query().Get("show")
	if !slices.Contains(showFilters, d.Show) {
		d.Show = ""
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
		case "graph":
			// Neighbors come from the host's cached resources; a scan's
			// result arrives over SSE.
			res := s.hostResources([]string{a.Host}, false)[a.Host]
			d.Graph = appGraph(a, res.out, s.C.Apps())
		case "containers", "images":
		case "configuration":
			groups, owner, err := s.appSettings(a, d.Admin)
			d.Settings = show(groups, d.Show, err)
			d.Settings.Origin, d.Settings.Owner = a.Origin, owner
		case "diff":
			d.Mode = r.URL.Query().Get("mode")
			if d.Mode == "objects" {
				d.All = r.URL.Query().Get("synced") == "1"
				d.Objects = s.objectDiffs(r.Context(), a, d.All, !d.Admin)
			} else {
				d.Mode = ""
			}
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
		if d.Tab == "diff" && d.Mode == "" {
			// The compact box sends full=0 ahead of a hidden full=1, so
			// unticking it leaves full=1.
			d.Full = r.URL.Query().Get("full") == "1"
			d.Files = fileDiffs(d.App, d.Full)
		}
	case "applicationsets":
		d.ApplicationSets = s.C.ApplicationSets()
	case "applicationset":
		found := false
		for _, set := range s.C.ApplicationSets() {
			if set.Name == d.Name {
				d.AppSet, found = set, true
			}
		}
		if !found {
			return nil, fmt.Errorf("applicationset %q: %w", d.Name, controller.ErrNotFound)
		}
		generated := s.generated(d.Name)
		for _, a := range s.C.Apps() {
			if generated[a.Name] {
				d.Apps = append(d.Apps, a)
			}
		}
		set, template, err := s.appSetSettings(d.Name, d.Admin)
		d.Settings, d.Template = show(set, d.Show, err), show(template, d.Show, err)
		d.Template.Prefix = templatePrefix
	case "hosts", "host":
		// Everything here renders from what is cached; scans of hosts whose
		// discovery is missing or stale start in the background.
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
		if d.Page == "host" && !d.Host.Retired {
			var err error
			if d.Operations, err = s.withLogs(s.C.HostOperations(d.Name, 10)); err != nil {
				return nil, err
			}
		}
		if d.Page == "host" && !d.Host.Retired {
			groups, origin, err := s.hostSettings(d.Name, d.Admin)
			d.Settings = show(groups, d.Show, err)
			d.Settings.Origin = origin
		}
		d.Inventory = s.inventories(scan, false)
		if d.Page == "host" && len(scan) == 1 {
			res := s.hostResources(scan, false)[d.Name]
			inv := d.Inventory[d.Name]
			if res.out != nil {
				g := hostGraph(d.Name, res.out, s.C.Apps())
				inv.Graph = &g
			}
			if res.err != nil {
				inv.GraphError = res.err.Error()
			}
			d.Inventory[d.Name] = inv
		}
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
		groups, err := s.serverSettings()
		d.Settings = show(groups, d.Show, err)
		if b := s.C.Bootstrap(); b != nil {
			d.Settings.Origin = b.Path
		}
	default:
		return nil, controller.ErrNotFound
	}
	d.Editing = r.URL.Query().Get("edit") == "1" && editable(d)
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
	return s.withLogs(s.C.Operations(app, 50))
}

// withLogs returns ops with their output, masked.
func (s *Server) withLogs(ops []*store.Operation, err error) ([]*store.Operation, error) {
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
	case errors.As(err, new(*auth.InvalidError)), errors.As(err, new(invalidRequest)), errors.Is(err, controller.ErrInvalidObject):
		code = http.StatusBadRequest
	case errors.Is(err, controller.ErrNotFound), errors.Is(err, store.ErrNotFound):
		code = 404
	case errors.Is(err, controller.ErrBusy), errors.Is(err, controller.ErrBlocked), errors.Is(err, controller.ErrAdoptionBlocked), errors.Is(err, store.ErrExists),
		errors.Is(err, controller.ErrHostBusy), errors.Is(err, controller.ErrNotHeld), errors.Is(err, controller.ErrNoOffer),
		errors.Is(err, controller.ErrOfferChanged), errors.Is(err, controller.ErrNotUpgradable):
		code = 409
	}
	http.Error(w, err.Error(), code)
}

// actionWait is how long an action that finishes in the background (a
// refresh, an image check, a rescan) holds its response to report the outcome.
// One that takes longer is reported as still running; the page's live updates
// show its result.
var actionWait = 30 * time.Second

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
	ctx, cancel := context.WithTimeout(r.Context(), actionWait)
	defer cancel()
	// toast is the outcome; late is it for an action still running at actionWait.
	var toast, late, token string
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
			toast = "Paused automated sync of " + name
		case "resume":
			err = s.C.Resume(name)
			toast = "Resumed automated sync of " + name
		case "refresh":
			err = s.C.RefreshAppWait(ctx, name)
			toast, late = "Refreshed "+name, "Still refreshing "+name+"; this page updates when it's done"
		case "check-images":
			var check controller.ImageCheck
			check, err = s.C.CheckImagesWait(ctx, name)
			toast, late = imageToast(check), "Still waiting on image registries; this page updates when they answer"
			if err == nil {
				err = imageErr(check)
			}
		case "image-upgrade":
			svc, choice, tag := r.FormValue("service"), r.FormValue("choice"), r.FormValue("tag")
			if choice != store.UpgradeApprove && choice != store.UpgradeSkip {
				err = invalidRequest("invalid choice")
				break
			}
			var u *store.ImageUpgrade
			if u, err = s.C.DecideUpgrade(name, svc, choice, tag, p.Name); err == nil {
				toast = fmt.Sprintf("Skipped %s for %s until a newer tag", u.Tag, svc)
				if choice == store.UpgradeApprove {
					toast = fmt.Sprintf("Approved the upgrade of %s to %s", svc, u.Tag)
				}
			}
		case "image-decision":
			svc, choice := r.FormValue("service"), r.FormValue("choice")
			if choice != store.DecisionMemory && choice != store.DecisionDismiss {
				err = invalidRequest("invalid choice")
				break
			}
			var d *store.ImageDecision
			if d, err = s.C.DecideImage(name, svc, choice, p.Name); err == nil {
				toast = fmt.Sprintf("Dismissed %s for %s until a newer tag", d.Candidate, svc)
				if choice == store.DecisionMemory {
					toast = fmt.Sprintf("Applying %s to %s in memory; it diverges from Git", d.Candidate, svc)
				}
			}
		default:
			err = controller.ErrNotFound
		}
	case strings.HasPrefix(r.URL.Path, "/ui/hosts/"):
		target = "/hosts/" + url.PathEscape(name)
		switch action {
		case "maintenance":
			target = "/hosts"
			on := r.FormValue("enabled") == "true"
			err = s.C.SetMaintenance(name, on, "maintenance (by "+p.Name+")")
			toast = "Ended maintenance on " + name
			if on {
				toast = "Started maintenance on " + name
			}
		case "rescan":
			err = s.rescan(ctx, name)
			toast, late = "Rescanned "+name, "Still scanning "+name+"; this page updates when it's done"
		case "prune":
			kind := r.FormValue("kind")
			if kind != store.KindImagePrune && kind != store.KindBuilderPrune {
				err = invalidRequest("invalid prune kind")
				break
			}
			op, err = s.C.PruneHost(name, kind, r.FormValue("all") == "true", p.Name)
		case "delete-release":
			op, err = s.C.DeleteRelease(name, r.FormValue("release"), p.Name)
			if err == nil {
				// Rescan once it's gone, so the release leaves the list, even
				// after this request has ended.
				go func(ctx context.Context, host string, id int64) {
					_ = s.C.FollowOperation(ctx, id, io.Discard)
					s.inventories([]string{host}, true)
				}(context.WithoutCancel(r.Context()), name, op.ID)
			}
		default:
			err = controller.ErrNotFound
		}
	case strings.HasPrefix(r.URL.Path, "/ui/root/"):
		target = "/apps"
		switch action {
		case "refresh":
			var root controller.RootStatus
			root, err = s.C.RefreshWait(ctx)
			toast, late = "Fetched the root repository", "Still fetching the root repository; this page updates when it's done"
			if rev := root.Revision; rev != "" {
				toast += " at " + rev[:min(len(rev), 7)]
			}
		case "check-images":
			var check controller.ImageCheck
			check, err = s.C.CheckImagesWait(ctx, "")
			toast, late = imageToast(check), "Still waiting on image registries; this page updates when they answer"
			if err == nil {
				err = imageErr(check)
			}
		default:
			err = controller.ErrNotFound
		}
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
			toast = "Revoked the token " + r.FormValue("name")
		case "user-add":
			var role auth.Role
			role, err = auth.ParseRole(r.FormValue("role"))
			if err == nil {
				err = s.Auth.AddUser(r.FormValue("name"), role, r.FormValue("password"))
			}
			toast = "Added " + r.FormValue("name") + " as " + string(role)
		case "user-password":
			err = s.Auth.SetPassword(r.FormValue("name"), r.FormValue("password"))
			toast = "Changed " + r.FormValue("name") + "'s password"
		case "user-delete":
			err = s.Auth.Store.DeleteUser(r.FormValue("name"))
			toast = "Deleted " + r.FormValue("name")
		default:
			err = controller.ErrNotFound
		}
	}
	// An action that outlasts actionWait carries on; the page shows how it ends.
	if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
		err, toast = nil, late
	}
	if err != nil {
		pageError(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		d := &Data{Toast: toast, Token: token, Op: op}
		if target == "/settings" {
			// Settings has no live updates, so the result re-renders it.
			r.URL.Path = "/settings"
			r.SetPathValue("name", "")
			if d, err = s.data(r, p); err != nil {
				pageError(w, err)
				return
			}
			d.OOB, d.Toast, d.Token = true, toast, token
		}
		switch {
		case op != nil:
			what := action
			if action == "prune" {
				what = op.Kind
			}
			d.Message = fmt.Sprintf("%s #%d", map[string]string{"sync": "Sync", "rollback": "Rollback", "delete-release": "Delete",
				store.KindImagePrune: "Image prune", store.KindBuilderPrune: "Build cache prune"}[what], op.ID)
		case token != "":
			d.Message = "Created the token " + r.FormValue("name")
		default:
			// Only the toast, and settings, change; a result already showing stays.
			w.Header().Set("HX-Reswap", "none")
		}
		render(w, 200, "result", d)
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
		d.Token, d.Message = token, "Created the token "+r.FormValue("name")
		render(w, 200, "page", d)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// rescan rediscovers host's releases and projects, returning once the scan
// finishes with its error, or ctx's error if that comes first.
func (s *Server) rescan(ctx context.Context, host string) error {
	if h, ok := s.hostStatus(host); !ok || h.Retired {
		return fmt.Errorf("host %q: %w", host, controller.ErrNotFound)
	}
	s.inventories([]string{host}, true)
	// The host graph's resources refresh alongside; they report over SSE.
	s.hostResources([]string{host}, true)
	s.inventoryMu.Lock()
	running := s.inventory[host].running
	s.inventoryMu.Unlock()
	if running != nil {
		select {
		case <-running:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	return s.inventory[host].err
}

// imageToast sums up an image check that every registry answered.
func imageToast(check controller.ImageCheck) string {
	var msg string
	switch n := len(check.Updated); {
	case check.Checked == 0:
		return "No images to check"
	case n == 0:
		msg = fmt.Sprintf("Checked %s; no new digests", plural(check.Checked, "image"))
	case n == 1:
		msg = fmt.Sprintf("Checked %s; %s has a new digest", plural(check.Checked, "image"), check.Updated[0])
	default:
		msg = fmt.Sprintf("Checked %s; %d have new digests", plural(check.Checked, "image"), n)
	}
	switch n := len(check.Moved); {
	case n == 1:
		msg += "; new tag for " + check.Moved[0]
	case n > 1:
		msg += fmt.Sprintf("; %d new tags", n)
	}
	switch n := len(check.Written); {
	case n == 1:
		msg += "; committed " + check.Written[0] + " to Git"
	case n > 1:
		msg += fmt.Sprintf("; committed %d tags to Git", n)
	}
	if n := len(check.Held); n > 0 {
		msg += fmt.Sprintf("; %s held: can't be written to Git", plural(n, "update"))
	}
	return msg
}

// imageErr is why the registries failed for an image check's tags, if any did.
func imageErr(check controller.ImageCheck) error {
	if len(check.Failed) == 0 {
		return nil
	}
	refs := make([]string, 0, len(check.Failed))
	for ref := range check.Failed {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %s couldn't be checked:", len(refs), plural(check.Checked, "image"))
	for _, ref := range refs {
		b.WriteString("\n" + ref + ": " + check.Failed[ref])
	}
	return errors.New(b.String())
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
		if d.View == "live" || d.View == "diff" {
			d.All = r.URL.Query().Get("all") == "1"
			if d.Live, err = s.C.LiveObject(r.Context(), d.Name, id, d.All, !d.Admin); err != nil {
				pageError(w, err)
				return
			}
		}
	}
	render(w, 200, name, d)
}

// HostDialog shows an engine object on a host, managed or not, as docker
// inspect sees it.
func (s *Server) HostDialog(w http.ResponseWriter, r *http.Request) {
	p := s.identify(w, r, auth.RoleReadonly)
	if p == nil {
		return
	}
	d := &Data{User: p, Admin: p.Role == auth.RoleAdmin, Name: r.PathValue("name"), Page: "dialog"}
	var err error
	if d.HostObject, err = s.C.HostObject(r.Context(), d.Name, r.PathValue("kind"), r.PathValue("object"), !d.Admin); err != nil {
		pageError(w, err)
		return
	}
	render(w, 200, "host-object", d)
}

// objectViews are the views an object's dialog offers.
func objectViews(o controller.Object) []string {
	views := []string{"summary"}
	switch {
	case o.Kind == controller.KindContainer, o.ID == "networks/default":
		views = append(views, "live")
	case o.Kind == controller.KindConfig, o.Kind == controller.KindSecret:
		views = append(views, "desired", "deployed")
	default:
		views = append(views, "live", "diff", "desired", "deployed")
	}
	return views
}

// ObjectDiff is one object's live state against Git, on the diff tab.
type ObjectDiff struct {
	Object controller.Object
	Live   *controller.Live
	Error  string
}

// objectDiffs compares each object that isn't synced (or every one, with
// all) with its live state. Containers are covered by their services.
func (s *Server) objectDiffs(ctx context.Context, a controller.AppStatus, all, redact bool) []ObjectDiff {
	order := map[controller.ObjectSync]int{controller.ObjectMissing: 0, controller.ObjectOutOfSync: 1, controller.ObjectPrune: 2, controller.ObjectSynced: 3}
	var out []ObjectDiff
	for _, o := range a.Objects {
		if o.Kind == controller.KindContainer || (!all && o.Sync == controller.ObjectSynced) {
			continue
		}
		d := ObjectDiff{Object: o}
		live, err := s.C.LiveObject(ctx, a.Name, o.ID, false, redact)
		if err != nil {
			d.Error = err.Error()
		}
		d.Live = live
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Object.Sync] < order[out[j].Object.Sync] })
	return out
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
