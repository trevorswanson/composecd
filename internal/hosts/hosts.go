// Package hosts keeps one connection setup per Docker engine: the docker CLI
// flags and environment that reach it, its release storage, and whether it is up.
package hosts

import (
	"context"
	"io"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/events"
)

// State is whether a host answered its last ping.
type State string

const (
	StateUnknown State = "Unknown"
	StateUp      State = "Up"
	StateDown    State = "Down"
)

// Status is a host as the API shows it.
type Status struct {
	Name           string            `json:"name"`
	Labels         map[string]string `json:"labels,omitempty"`
	URL            string            `json:"url,omitempty"`
	Context        string            `json:"context,omitempty"`
	State          State             `json:"state"`
	Error          string            `json:"error,omitempty"`
	DockerVersion  string            `json:"dockerVersion,omitempty"`
	ComposeVersion string            `json:"composeVersion,omitempty"`
	CheckedAt      time.Time         `json:"checkedAt,omitempty"`
}

// Backend creates what talks to an engine. The default runs the docker CLI;
// tests substitute fakes.
type Backend interface {
	// Runner returns a compose runner for h whose docker calls get env, and whose
	// progress output goes to progress.
	Runner(h *Host, env []string, progress io.Writer) compose.Runner
	// Store returns a fresh release storage driver for h.
	Store(h *Host) storage.Driver
	// Watch calls on with the compose project of every container event on h
	// until ctx is done or the event stream ends.
	Watch(ctx context.Context, h *Host, on func(project string)) error
}

// Host is one engine.
type Host struct {
	Spec    *config.Host
	backend Backend

	// ops is held for reading by every operation using the host and for writing
	// by Invalidate, so a storage cache is never swapped under an operation.
	ops    sync.RWMutex
	mu     sync.Mutex
	store  storage.Driver
	status Status
}

// Use marks the start of an operation that reads or writes releases on the
// host; call the returned function when it ends.
func (h *Host) Use() func() {
	h.ops.RLock()
	return h.ops.RUnlock
}

// Name is the host's name.
func (h *Host) Name() string { return h.Spec.Metadata.Name }

// Global returns the docker flags that select the engine.
func (h *Host) Global() []string {
	switch {
	case h.Spec.Spec.URL != "":
		return []string{"--host", h.Spec.Spec.URL}
	case h.Spec.Spec.Context != "":
		return []string{"--context", h.Spec.Spec.Context}
	}
	return nil
}

// Env returns the host's environment for docker calls: TLS settings and Spec.Env.
func (h *Host) Env() []string {
	var env []string
	if p := h.Spec.Spec.TLSCertPath; p != "" {
		env = append(env, "DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH="+p)
	}
	keys := make([]string, 0, len(h.Spec.Spec.Env))
	for k := range h.Spec.Spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+h.Spec.Spec.Env[k])
	}
	return env
}

// ProjectDir is app's compose project directory on the engine's machine.
func (h *Host) ProjectDir(app string) string {
	root := h.Spec.Spec.ProjectRoot
	if root == "" {
		root = config.DefaultProjectRoot
	}
	if strings.Contains(root, `\`) {
		return root + `\` + app
	}
	return path.Join(root, app)
}

// Store returns the host's release storage. Calls through it are serialized,
// since drivers keep an unsynchronized cache.
func (h *Host) Store() storage.Driver {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.store == nil {
		h.store = &locked{d: h.backend.Store(h)}
	}
	return h.store
}

// Invalidate drops the storage driver's cache, so releases changed by other
// clients (such as the composetpl CLI) are read again. While an operation is
// using the host (Use) it does nothing and returns false.
func (h *Host) Invalidate() bool {
	if !h.ops.TryLock() {
		return false
	}
	defer h.ops.Unlock()
	h.mu.Lock()
	h.store = nil
	h.mu.Unlock()
	return true
}

// Configuration returns a composetpl configuration for one operation on the
// host, with env added to every docker call and output going to log.
func (h *Host) Configuration(env []string, log io.Writer) *action.Configuration {
	return &action.Configuration{
		Store:   h.Store(),
		Compose: h.backend.Runner(h, append(h.Env(), env...), log),
		Log:     log,
	}
}

// Ping checks the engine and records the result in Status.
func (h *Host) Ping(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r := h.backend.Runner(h, h.Env(), io.Discard)
	s := h.baseStatus()
	s.CheckedAt = time.Now()
	cv, err := r.Version(ctx)
	if err == nil {
		s.ComposeVersion = cv
		s.DockerVersion, err = r.ServerVersion(ctx)
	}
	if err != nil {
		s.State, s.Error = StateDown, err.Error()
	} else {
		s.State = StateUp
	}
	h.mu.Lock()
	h.status = s
	h.mu.Unlock()
	return s
}

// Status is the result of the last Ping.
func (h *Host) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.status.Name == "" {
		return h.baseStatus()
	}
	return h.status
}

func (h *Host) baseStatus() Status {
	return Status{Name: h.Name(), Labels: h.Spec.Metadata.Labels, URL: h.Spec.Spec.URL, Context: h.Spec.Spec.Context, State: StateUnknown}
}

// Registry holds the hosts defined in Git.
type Registry struct {
	Backend Backend

	mu    sync.RWMutex
	hosts map[string]*Host
}

// NewRegistry returns an empty registry using backend (nil: the docker CLI).
func NewRegistry(backend Backend) *Registry {
	return &Registry{Backend: backend, hosts: map[string]*Host{}}
}

// Sync makes the registry match specs. A host whose spec changed is recreated.
func (r *Registry) Sync(specs []*config.Host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]*Host{}
	for _, s := range specs {
		name := s.Metadata.Name
		// An unchanged host keeps its object (and storage cache) untouched: other
		// goroutines read its Spec.
		if old, ok := r.hosts[name]; ok && reflect.DeepEqual(old.Spec.Spec, s.Spec) && reflect.DeepEqual(old.Spec.Metadata, s.Metadata) {
			next[name] = old
			continue
		}
		next[name] = &Host{Spec: s, backend: r.Backend}
	}
	r.hosts = next
}

// Get returns the host named name, or nil.
func (r *Registry) Get(name string) *Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hosts[name]
}

// All returns every host, sorted by name.
func (r *Registry) All() []*Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Host, 0, len(r.hosts))
	for _, h := range r.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// CLI is the default Backend: the docker CLI, with release history in the
// composetpl volume on each engine, or under DataDir for storage: local.
type CLI struct {
	// DataDir holds local release storage, under hosts/<name>.
	DataDir string
	// HelperImage overrides the volume driver's helper image.
	HelperImage string
}

func (c *CLI) Runner(h *Host, env []string, progress io.Writer) compose.Runner {
	cli := compose.NewCLI(h.Global(), progress)
	cli.Env = env
	return cli
}

func (c *CLI) Store(h *Host) storage.Driver {
	if h.Spec.Spec.Storage == config.StorageLocal {
		return storage.NewLocal(filepath.Join(c.DataDir, "hosts", h.Name(), "releases"))
	}
	v := storage.NewVolume(h.Global(), c.HelperImage)
	v.Env = h.Env()
	return v
}

func (c *CLI) Watch(ctx context.Context, h *Host, on func(project string)) error {
	return events.Watch(ctx, h.Global(), h.Env(), on)
}

// locked serializes a storage driver.
type locked struct {
	mu sync.Mutex
	d  storage.Driver
}

func (l *locked) Name() string { return l.d.Name() }

func (l *locked) Get(name string, revision int) (*release.Release, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Get(name, revision)
}

func (l *locked) History(name string) ([]*release.Release, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.History(name)
}

func (l *locked) List() ([]*release.Release, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.List()
}

func (l *locked) Save(rels ...*release.Release) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Save(rels...)
}

func (l *locked) Delete(rels ...*release.Release) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Delete(rels...)
}
