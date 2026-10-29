// Package controller reconciles the Applications in Git with composetpl
// releases on their hosts: it refreshes Git, compares, assesses health, and
// syncs automatically or on request.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composetpl/pkg/registry"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/infisical"
	"github.com/trevorswanson/composecd/internal/masking"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

// Errors returned to API callers.
var (
	ErrNotFound = errors.New("not found")
	ErrBusy     = errors.New("an operation is already running for this application")
	// ErrBlocked wraps a sync window's refusal of a manual operation.
	ErrBlocked = errors.New("blocked by a sync window")
)

// Options are a Controller's collaborators.
type Options struct {
	Boot  *config.Bootstrap
	Git   *gitcache.Cache
	Hosts *hosts.Registry
	Store *store.Store
	// Registry pulls oci:// charts; nil uses a default client.
	Registry *registry.Client
	Log      *slog.Logger
	// Notify receives events (failed syncs, degraded apps, ...); nil drops them.
	Notify Notifier
}

// Controller reconciles every Application.
type Controller struct {
	Options
	changes      subscriptions
	maskMu       sync.RWMutex
	secretValues map[string]map[string]bool
	secretPaths  map[string]map[string][]string
	maskers      map[string]*masking.Masker
	infisicalMu  sync.Mutex
	infisicalFor *config.Infisical
	infisicalC   *infisical.Client

	// ctx is the Run context; operations started from the API use it.
	ctx       context.Context
	refreshCh chan struct{}
	sem       chan struct{}
	ops       tracker

	mu           sync.RWMutex
	objects      *config.Objects
	appsets      []ApplicationSetStatus
	retiredHosts map[string]*hosts.Host
	root         RootStatus
	apps         map[string]*appState
	running      map[string]int64
	// liveLogs are running operations' output, by operation ID, so it can be
	// followed before the operation finishes.
	liveLogs map[int64]*logBuffer
	appLocks map[string]*sync.Mutex
	// checkouts counts operations using each checkout, so Prune keeps them.
	checkouts map[string]int
	windows   []*windows.Window
	// orphans are, per host, releases left behind that weren't pruned.
	orphans  map[string][]string
	timers   map[string]*timer
	metrics  map[metricKey]*opMetric
	watchers map[string]*watcher
}

// watcher streams one host's container events.
type watcher struct {
	host   *hosts.Host
	cancel context.CancelFunc
}

// eventDebounce coalesces the burst of events a container change causes.
var eventDebounce = 2 * time.Second

type appState struct {
	status AppStatus
	// attempted is the desired-state key auto-sync last tried, so a failing
	// sync is not retried until Git (or the host) changes, except by Retry.
	attempted string
	failures  int
	nextRetry time.Time
	// heals counts self-heals in a row, for backoff.
	heals    int
	nextHeal time.Time
}

// infisicalClient returns the shared client for the bootstrap's infisical
// section, so the Universal Auth session is reused across reconciles. It is nil
// when the section is absent.
func (c *Controller) infisicalClient() *infisical.Client {
	c.infisicalMu.Lock()
	defer c.infisicalMu.Unlock()
	if c.Boot == nil || c.Boot.Infisical == nil {
		return nil
	}
	if c.infisicalFor != c.Boot.Infisical {
		c.infisicalFor, c.infisicalC = c.Boot.Infisical, infisical.New(c.Boot.Infisical)
	}
	return c.infisicalC
}

// New returns a Controller. Call Run to start reconciling.
func New(o Options) *Controller {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Controller{
		Options:   o,
		ctx:       context.Background(),
		refreshCh: make(chan struct{}, 1),
		sem:       make(chan struct{}, o.Boot.Concurrency),
		apps:      map[string]*appState{},
		running:   map[string]int64{},
		liveLogs:  map[int64]*logBuffer{},
		appLocks:  map[string]*sync.Mutex{},
		checkouts: map[string]int{},
		orphans:   map[string][]string{},
		timers:    map[string]*timer{},
		metrics:   map[metricKey]*opMetric{},
		watchers:  map[string]*watcher{},
		root:      RootStatus{Root: o.Boot.Root},
	}
}

// tracker counts background operations. Unlike a sync.WaitGroup, Add may be
// called while another goroutine Waits (retry timers start work at any time).
type tracker struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
}

func (t *tracker) Add(n int) {
	t.mu.Lock()
	t.n += n
	t.mu.Unlock()
}

func (t *tracker) Done() {
	t.mu.Lock()
	if t.n--; t.n <= 0 {
		t.n = 0
		if t.cond != nil {
			t.cond.Broadcast()
		}
	}
	t.mu.Unlock()
}

// Wait blocks until no operation is running.
func (t *tracker) Wait() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cond == nil {
		t.cond = sync.NewCond(&t.mu)
	}
	for t.n > 0 {
		t.cond.Wait()
	}
}

func (c *Controller) notify(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if c.Notify != nil {
		e.Message = c.Mask(e.App, e.Message)
		c.Notify.Notify(e)
	}
}

// Run refreshes immediately and then every RefreshInterval, or when Refresh is
// called, until ctx is done. It waits for running operations before returning.
func (c *Controller) Run(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	t := time.NewTicker(time.Duration(c.Boot.RefreshInterval))
	defer t.Stop()
	for {
		c.RefreshAll(ctx)
		select {
		case <-ctx.Done():
			c.Close()
			c.ops.Wait()
			return
		case <-t.C:
		case <-c.refreshCh:
		}
	}
}

// Close stops event watchers and pending retries. Run calls it when its
// context ends.
func (c *Controller) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, t := range c.timers {
		t.timer.Stop()
		delete(c.timers, name)
	}
	for name, w := range c.watchers {
		w.cancel()
		delete(c.watchers, name)
	}
}

// watchHosts keeps one event watcher per host, restarting it when the host's
// definition changes.
func (c *Controller) watchHosts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := map[string]*hosts.Host{}
	for _, h := range c.Hosts.All() {
		current[h.Name()] = h
	}
	for name, w := range c.watchers {
		if current[name] != w.host {
			w.cancel()
			delete(c.watchers, name)
		}
	}
	for name, h := range current {
		if _, ok := c.watchers[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(c.ctx)
		c.watchers[name] = &watcher{host: h, cancel: cancel}
		go c.watch(ctx, h)
	}
}

// watch streams h's events, reconnecting with backoff, until ctx is done.
func (c *Controller) watch(ctx context.Context, h *hosts.Host) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.Hosts.Backend.Watch(ctx, h, func(project string) { c.onEvent(h.Name(), project) })
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.Log.Debug("container event stream ended; reconnecting", "host", h.Name(), "in", backoff, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Minute)
	}
}

// onEvent reconciles the app a container event on host is about, shortly.
func (c *Controller) onEvent(host, project string) {
	app, err := c.application(project)
	if err != nil || app.Spec.Destination.Host != host {
		return
	}
	c.mu.Lock()
	c.scheduleLocked(project, time.Now().Add(eventDebounce))
	c.mu.Unlock()
}

// Wait blocks until every operation started in the background (syncs,
// rollbacks, app refreshes) has finished.
func (c *Controller) Wait() { c.ops.Wait() }

// Refresh asks Run to refresh everything now.
func (c *Controller) Refresh() {
	select {
	case c.refreshCh <- struct{}{}:
	default:
	}
}

func (c *Controller) baseContext() context.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ctx
}

// RefreshAll fetches Git, reloads the objects, pings every host and reconciles
// every Application, auto-syncing those that want it.
func (c *Controller) RefreshAll(ctx context.Context) {
	defer c.publish("app", "")
	objs, sha, err := c.loadRoot(ctx)
	rootErr := err
	var generatorFetches map[string]error
	var setErr error
	if objs != nil {
		var statuses []ApplicationSetStatus
		var expansionErr error
		objs, statuses, generatorFetches, expansionErr = c.expand(ctx, objs)
		err = errors.Join(err, expansionErr)
		setErr = setErrors(statuses)
		c.mu.Lock()
		c.appsets = statuses
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.root.Revision, c.root.RefreshedAt, c.root.Error = sha, time.Now(), ""
	if reported := errors.Join(err, setErr); reported != nil {
		c.root.Error = reported.Error()
		c.Log.Error("refreshing the root", "repo", c.Boot.Root.RepoURL, "error", reported)
	}
	// A failed fetch still yields the objects at the cached revision. Prune only
	// runs on objects read fresh from Git. A failed ApplicationSet alone doesn't
	// stop prune: setPruneGuard protects its own releases.
	fresh := err == nil
	if objs != nil {
		c.objects = objs
		c.windows = windows.Compile(objs.SyncWindows)
	}
	objs = c.objects
	c.mu.Unlock()
	if objs == nil {
		return
	}

	previousHosts := c.Hosts.All()
	c.Hosts.Sync(objs.Hosts)
	c.mu.Lock()
	if c.retiredHosts == nil {
		c.retiredHosts = map[string]*hosts.Host{}
	}
	for _, h := range previousHosts {
		if c.Hosts.Get(h.Name()) == nil {
			c.retiredHosts[h.Name()] = h
		}
	}
	for _, h := range objs.Hosts {
		delete(c.retiredHosts, h.Metadata.Name)
	}
	c.mu.Unlock()
	c.watchHosts()
	var wg sync.WaitGroup
	for _, h := range c.Hosts.All() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Invalidate()
			prev := h.Status().State
			defer c.publish("host", h.Name())
			if s := h.Ping(ctx); s.State != hosts.StateUp {
				c.Log.Warn("host is down", "host", s.Name, "error", s.Error)
				if prev == hosts.StateUp {
					c.notify(Event{Type: config.EventHostDown, Host: s.Name, Message: "host " + s.Name + " is down: " + s.Error})
				}
			}
		}()
	}
	wg.Wait()
	c.restoreEmptyGuards()

	// The root was fetched by loadRoot; fetch every other source repository once.
	fetchErrs := generatorFetches
	if fetchErrs == nil {
		fetchErrs = map[string]error{}
	}
	fetchErrs[c.Boot.Root.RepoURL] = rootErr
	for _, a := range objs.Applications {
		url := sourceRepo(a)
		if url == "" || url == c.Boot.Root.RepoURL {
			continue
		}
		if _, done := fetchErrs[url]; !done {
			fetchErrs[url] = c.Git.Fetch(ctx, url)
			if fetchErrs[url] != nil {
				c.Log.Error("fetching a source repository", "repo", url, "error", fetchErrs[url])
			}
		}
	}

	c.mu.Lock()
	for name := range c.apps {
		if objs.Application(name) == nil {
			delete(c.apps, name)
			if t, ok := c.timers[name]; ok {
				t.timer.Stop()
				delete(c.timers, name)
			}
		}
	}
	c.mu.Unlock()

	// Reconcile in dependency waves, so an app synced in this refresh unblocks
	// the apps that depend on it in the same refresh.
	for _, wave := range objs.Waves() {
		for _, a := range wave {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.sem <- struct{}{}
				defer func() { <-c.sem }()
				c.reconcile(ctx, a, fetchErrs[sourceRepo(a)], true)
			}()
		}
		wg.Wait()
	}
	c.prune(ctx, objs, fresh)
	c.pruneCheckouts(sha)
}

// loadRoot fetches the root repository and reads the objects at its revision.
func (c *Controller) loadRoot(ctx context.Context) (*config.Objects, string, error) {
	root := c.Boot.Root
	fetchErr := c.Git.Fetch(ctx, root.RepoURL)
	sha, err := c.Git.Resolve(ctx, root.RepoURL, root.TargetRevision)
	if err != nil {
		if fetchErr != nil {
			return nil, "", fetchErr
		}
		return nil, "", err
	}
	dir, err := c.Git.Checkout(ctx, root.RepoURL, sha)
	if err != nil {
		return nil, sha, err
	}
	objs, err := config.LoadDir(filepath.Join(dir, filepath.FromSlash(root.Path)))
	if err != nil {
		return nil, sha, fmt.Errorf("reading objects at %s: %w", short(sha), err)
	}
	objs.Resolve(root)
	return objs, sha, fetchErr
}

// pruneCheckouts deletes checkouts nothing refers to any more.
func (c *Controller) pruneCheckouts(rootSHA string) {
	c.mu.RLock()
	keep := map[string]bool{}
	if rootSHA != "" {
		keep[gitcache.CheckoutKey(c.Boot.Root.RepoURL, rootSHA)] = true
	}
	for _, st := range c.apps {
		r := st.status.Sync.Revision
		if st.status.Source.IsOCI() {
			r = st.status.Sync.ValuesRevision
		}
		url, _ := st.status.Source.GitRepository()
		if r != "" && url != "" {
			keep[gitcache.CheckoutKey(url, r)] = true
		}
	}
	for k, n := range c.checkouts {
		if n > 0 {
			keep[k] = true
		}
	}
	c.mu.RUnlock()
	if err := c.Git.Prune(keep); err != nil {
		c.Log.Warn("pruning git checkouts", "error", err)
	}
}

// useCheckout keeps a checkout from being pruned until the returned function runs.
func (c *Controller) useCheckout(url, sha string) func() {
	k := gitcache.CheckoutKey(url, sha)
	c.mu.Lock()
	c.checkouts[k]++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if c.checkouts[k]--; c.checkouts[k] <= 0 {
			delete(c.checkouts, k)
		}
		c.mu.Unlock()
	}
}

// lockApp serializes everything that touches one application.
func (c *Controller) lockApp(name string) func() {
	c.mu.Lock()
	l, ok := c.appLocks[name]
	if !ok {
		l = &sync.Mutex{}
		c.appLocks[name] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (c *Controller) application(name string) (*config.Application, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.objects == nil {
		return nil, fmt.Errorf("application %q: %w", name, ErrNotFound)
	}
	if a := c.objects.Application(name); a != nil {
		return a, nil
	}
	return nil, fmt.Errorf("application %q: %w", name, ErrNotFound)
}

// setStatus records st and notifies about transitions into a bad state.
func (c *Controller) setStatus(st *AppStatus) {
	c.mu.Lock()
	s, ok := c.apps[st.Name]
	if !ok {
		s = &appState{}
		c.apps[st.Name] = s
	}
	prev := s.status
	s.status = *st
	c.mu.Unlock()
	c.publish("app", st.Name)
	if !ok || prev.Name == "" {
		return
	}
	if st.Health.Status == health.Degraded && prev.Health.Status != health.Degraded && prev.Health.Status != health.Unknown {
		c.notify(Event{Type: config.EventHealthDegraded, App: st.Name, Host: st.Host, Revision: st.Sync.DeployedRevision,
			Message: fmt.Sprintf("%s is degraded: %s", st.Name, st.Health.Message)})
	}
	if st.Sync.Status == OutOfSync && prev.Sync.Status == Synced {
		c.notify(Event{Type: config.EventOutOfSync, App: st.Name, Host: st.Host, Revision: st.Sync.Revision,
			Message: fmt.Sprintf("%s is out of sync: %s", st.Name, strings.Join(st.Sync.Reasons, "; "))})
	}
}

// Apps returns every Application's status, sorted by name. Apps not yet
// reconciled are listed with Unknown status.
func (c *Controller) Apps() []AppStatus {
	c.mu.RLock()
	var out []AppStatus
	if c.objects != nil {
		for _, a := range c.objects.Applications {
			if s, ok := c.apps[a.Metadata.Name]; ok {
				out = append(out, s.status)
			} else {
				out = append(out, pendingStatus(a))
			}
		}
	}
	c.mu.RUnlock()
	for i := range out {
		out[i] = c.maskStatus(c.withRunning(out[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// App returns one application's status with secrets redacted for API/UI use.
func (c *Controller) App(name string) (AppStatus, error) {
	a, err := c.application(name)
	if err != nil {
		return AppStatus{}, err
	}
	c.mu.RLock()
	st := pendingStatus(a)
	if s, ok := c.apps[name]; ok {
		st = s.status
	}
	c.mu.RUnlock()
	return c.maskStatus(c.withRunning(st)), nil
}

// withRunning shows st's running operation, if any, as its last one: status
// is recorded after each reconcile, which lags an operation that just started.
func (c *Controller) withRunning(st AppStatus) AppStatus {
	c.mu.RLock()
	id := c.running[st.Name]
	c.mu.RUnlock()
	if id == 0 || (st.Operation != nil && st.Operation.ID == id) {
		return st
	}
	if op, err := c.Store.Operation(id); err == nil {
		op.Log = ""
		st.Operation = op
	}
	return st
}

func pendingStatus(a *config.Application) AppStatus {
	st := baseStatus(a)
	return *st
}

// Hosts returns every host's status, sorted by name.
func (c *Controller) HostStatuses() []HostStatus {
	counts := map[string]int{}
	c.mu.RLock()
	if c.objects != nil {
		for _, a := range c.objects.Applications {
			counts[a.Spec.Destination.Host]++
		}
	}
	c.mu.RUnlock()
	c.mu.RLock()
	orphans := c.orphans
	c.mu.RUnlock()
	var out []HostStatus
	for _, h := range c.pruneHosts() {
		hs := HostStatus{Status: h.Status(), Apps: counts[h.Name()], Orphans: orphans[h.Name()], Retired: c.Hosts.Get(h.Name()) == nil}
		if st, err := c.Store.HostState(h.Name()); err == nil {
			hs.Maintenance, hs.MaintenanceReason = st.Maintenance, st.MaintenanceReason
		}
		out = append(out, hs)
	}
	return out
}

// SetMaintenance turns maintenance mode on or off for a host. While it is on,
// automatic syncs, self-heal and prune skip the host; manual operations work.
func (c *Controller) SetMaintenance(host string, on bool, reason string) error {
	if c.Hosts.Get(host) == nil {
		return fmt.Errorf("host %q: %w", host, ErrNotFound)
	}
	if on && reason == "" {
		reason = "maintenance"
	}
	if err := c.Store.SetMaintenance(host, on, reason); err != nil {
		return err
	}
	c.publish("host", host)
	if !on {
		// Catch up on whatever maintenance held back.
		c.mu.RLock()
		var apps []*config.Application
		if c.objects != nil {
			for _, a := range c.objects.Applications {
				if a.Spec.Destination.Host == host {
					apps = append(apps, a)
				}
			}
		}
		c.mu.RUnlock()
		for _, a := range apps {
			c.reconcileAsync(a, false)
		}
	}
	return nil
}

// Windows returns every SyncWindow and whether it is open.
func (c *Controller) Windows() []windows.Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var apps []*config.Application
	if c.objects != nil {
		apps = c.objects.Applications
	}
	return windows.Statuses(c.windows, apps, time.Now())
}

// Root returns the state of the Git root.
func (c *Controller) Root() RootStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.root
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// RefreshRepos triggers a refresh if any of urls (from a Git push webhook) is
// the root repository or an Application's, and reports whether it did.
func (c *Controller) RefreshRepos(urls []string) bool {
	want := map[string]bool{NormalizeRepoURL(c.Boot.Root.RepoURL): true}
	c.mu.RLock()
	if c.objects != nil {
		for _, a := range c.objects.Applications {
			if url := sourceRepo(a); url != "" {
				want[NormalizeRepoURL(url)] = true
			}
		}
		for _, set := range c.objects.ApplicationSets {
			for _, g := range set.Spec.Generators {
				if d := g.GitDirectories; d != nil {
					want[NormalizeRepoURL(d.RepoURL)] = true
				}
			}
		}
	}
	c.mu.RUnlock()
	for _, u := range urls {
		if u != "" && want[NormalizeRepoURL(u)] {
			c.Refresh()
			return true
		}
	}
	return false
}

// NormalizeRepoURL reduces the ways one repository can be written
// (https://git.lan/me/x.git, git@git.lan:me/x, ssh://git@git.lan/me/x) to
// "git.lan/me/x".
func NormalizeRepoURL(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else if at, colon := strings.Index(u, "@"), strings.Index(u, ":"); colon > at && !strings.Contains(u[:colon], "/") {
		// scp-like git@host:path
		u = u[:colon] + "/" + u[colon+1:]
	}
	if i := strings.Index(u, "@"); i >= 0 && i < strings.IndexByte(u+"/", '/') {
		u = u[i+1:]
	}
	if host, rest, ok := strings.Cut(u, "/"); ok {
		// Drop a port: ssh and https URLs of one repository use different ones.
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
		u = host + "/" + rest
	}
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	return u
}

// sourceRepo is the source Git repository, including OCI valuesFrom.
func sourceRepo(app *config.Application) string {
	url, _ := app.Spec.Source.GitRepository()
	return url
}

func (c *Controller) fetchSource(ctx context.Context, app *config.Application) error {
	if url := sourceRepo(app); url != "" {
		return c.Git.Fetch(ctx, url)
	}
	return nil
}
