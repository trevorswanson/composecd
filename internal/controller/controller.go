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
	"sync"
	"time"

	"github.com/trevorswanson/composetpl/pkg/registry"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

// Errors returned to API callers.
var (
	ErrNotFound = errors.New("not found")
	ErrBusy     = errors.New("an operation is already running for this application")
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
}

// Controller reconciles every Application.
type Controller struct {
	Options

	// ctx is the Run context; operations started from the API use it.
	ctx       context.Context
	refreshCh chan struct{}
	sem       chan struct{}
	ops       sync.WaitGroup

	mu       sync.RWMutex
	objects  *config.Objects
	root     RootStatus
	apps     map[string]*appState
	running  map[string]int64
	appLocks map[string]*sync.Mutex
	// checkouts counts operations using each checkout, so Prune keeps them.
	checkouts map[string]int
}

type appState struct {
	status AppStatus
	// attempted is the desired-state key auto-sync last tried, so a failing
	// sync is not retried until Git (or the host) changes.
	attempted string
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
		appLocks:  map[string]*sync.Mutex{},
		checkouts: map[string]int{},
		root:      RootStatus{Root: o.Boot.Root},
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
			c.ops.Wait()
			return
		case <-t.C:
		case <-c.refreshCh:
		}
	}
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
	objs, sha, err := c.loadRoot(ctx)
	c.mu.Lock()
	c.root.Revision, c.root.RefreshedAt, c.root.Error = sha, time.Now(), ""
	if err != nil {
		c.root.Error = err.Error()
		c.Log.Error("refreshing the root", "repo", c.Boot.Root.RepoURL, "error", err)
	}
	// A failed fetch still yields the objects at the cached revision.
	if objs != nil {
		c.objects = objs
	}
	objs = c.objects
	c.mu.Unlock()
	if objs == nil {
		return
	}

	c.Hosts.Sync(objs.Hosts)
	var wg sync.WaitGroup
	for _, h := range c.Hosts.All() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Invalidate()
			if s := h.Ping(ctx); s.State != hosts.StateUp {
				c.Log.Warn("host is down", "host", s.Name, "error", s.Error)
			}
		}()
	}
	wg.Wait()

	// The root was fetched by loadRoot; fetch every other source repository once.
	fetchErrs := map[string]error{}
	for _, a := range objs.Applications {
		url := a.Spec.Source.RepoURL
		if a.Spec.Source.IsOCI() || url == c.Boot.Root.RepoURL {
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
		}
	}
	c.mu.Unlock()

	for _, a := range objs.Applications {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.sem <- struct{}{}
			defer func() { <-c.sem }()
			c.reconcile(ctx, a, fetchErrs[a.Spec.Source.RepoURL], true)
		}()
	}
	wg.Wait()
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
		if r := st.status.Sync.Revision; r != "" {
			keep[gitcache.CheckoutKey(st.status.Source.RepoURL, r)] = true
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

func (c *Controller) setStatus(st *AppStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.apps[st.Name]
	if !ok {
		s = &appState{}
		c.apps[st.Name] = s
	}
	s.status = *st
}

// Apps returns every Application's status, sorted by name. Apps not yet
// reconciled are listed with Unknown status.
func (c *Controller) Apps() []AppStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []AppStatus
	if c.objects == nil {
		return out
	}
	for _, a := range c.objects.Applications {
		if s, ok := c.apps[a.Metadata.Name]; ok {
			out = append(out, s.status)
		} else {
			out = append(out, pendingStatus(a))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// App returns one Application's status.
func (c *Controller) App(name string) (AppStatus, error) {
	a, err := c.application(name)
	if err != nil {
		return AppStatus{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.apps[name]; ok {
		return s.status, nil
	}
	return pendingStatus(a), nil
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
	var out []HostStatus
	for _, h := range c.Hosts.All() {
		out = append(out, HostStatus{Status: h.Status(), Apps: counts[h.Name()]})
	}
	return out
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
