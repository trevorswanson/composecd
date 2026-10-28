package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/registry"
	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"
	"sigs.k8s.io/yaml"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
)

// ErrAdoptionBlocked reports a release that cannot be exported safely.
var ErrAdoptionBlocked = errors.New("adoption is blocked")

// Unmanaged is a host's release and plain Compose inventory, without values.
type Unmanaged struct {
	Releases []UnmanagedRelease `json:"releases"`
	Projects []UnmanagedProject `json:"projects"`
}

type UnmanagedRelease struct {
	Name       string `json:"name"`
	Revision   int    `json:"revision"`
	Status     string `json:"status"`
	ChartRef   string `json:"chartRef,omitempty"`
	ProjectDir string `json:"projectDir,omitempty"`
	Orphaned   bool   `json:"orphaned,omitempty"`
	// Blocked explains why an Application cannot be suggested for this release.
	Blocked string `json:"blocked,omitempty"`
}

type UnmanagedProject struct {
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	ConfigFiles []string `json:"configFiles"`
	Hint        string   `json:"hint"`
}

// Adoption is a suggestion only. YAML contains the release's unmasked user
// values and must only be returned to admins. No release or Git data is changed.
type Adoption struct {
	YAML     string   `json:"yaml"`
	Warnings []string `json:"warnings"`
}

func (c *Controller) adoptionHost(name string) (*hosts.Host, error) {
	h := c.Hosts.Get(name)
	if h == nil {
		return nil, fmt.Errorf("host %q: %w", name, ErrNotFound)
	}
	return h, nil
}

func (c *Controller) adoptionBlocked(name string, rel *release.Release) string {
	if rel.Status.IsPending() {
		return "a composetpl operation is pending"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.objects != nil && c.objects.Application(name) != nil {
		return "an Application with this name is already defined"
	}
	return ""
}

// Unmanaged discovers externally installed releases and plain Compose projects.
// Orphaned composecd releases are included so they can be brought back into Git.
func (c *Controller) Unmanaged(ctx context.Context, host string) (*Unmanaged, error) {
	h, err := c.adoptionHost(host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	h.Invalidate()
	defer h.Use()()
	cfg := h.Configuration(nil, io.Discard)
	rels, err := cfg.Store.List()
	if err != nil {
		return nil, fmt.Errorf("listing releases on %s: %w", host, err)
	}
	out := &Unmanaged{Releases: []UnmanagedRelease{}, Projects: []UnmanagedProject{}}
	names := map[string]bool{}
	for _, rel := range rels {
		if rel.Status == release.StatusUninstalled {
			continue
		}
		names[rel.Name] = true
		c.mu.RLock()
		var app *config.Application
		if c.objects != nil {
			app = c.objects.Application(rel.Name)
		}
		c.mu.RUnlock()
		if app != nil && app.Spec.Destination.Host == host {
			continue
		}
		out.Releases = append(out.Releases, UnmanagedRelease{Name: rel.Name, Revision: rel.Revision, Status: string(rel.Status), ChartRef: rel.Chart.Ref,
			ProjectDir: rel.ProjectDir, Orphaned: rel.Labels[LabelManagedBy] == ManagedBy, Blocked: c.adoptionBlocked(rel.Name, rel)})
	}
	projects, err := cfg.Compose.Ls(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing Compose projects on %s: %w", host, err)
	}
	for _, p := range projects {
		if names[p.Name] {
			continue
		}
		// The pinned composetpl Runner.Ls omits ConfigFiles. Compose also records
		// them on container labels, which PS exposes for running and stopped projects.
		containers, err := cfg.Compose.PS(ctx, p.Name)
		if err != nil {
			return nil, fmt.Errorf("reading project %s on %s: %w", p.Name, host, err)
		}
		files := map[string]bool{}
		for _, container := range containers {
			for _, f := range strings.Split(container.Labels["com.docker.compose.project.config_files"], ",") {
				if f = strings.TrimSpace(f); f != "" {
					files[f] = true
				}
			}
		}
		paths := make([]string, 0, len(files))
		for f := range files {
			paths = append(paths, f)
		}
		sort.Strings(paths)
		out.Projects = append(out.Projects, UnmanagedProject{Name: p.Name, Status: p.Status, ConfigFiles: paths, Hint: "Convert to a chart with composetpl create; automatic conversion is not supported."})
	}
	sort.Slice(out.Releases, func(i, j int) bool { return out.Releases[i].Name < out.Releases[j].Name })
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Name < out.Projects[j].Name })
	return out, nil
}

// Adopt exports an Application skeleton, retaining the name for an in-place
// upgrade when the user commits it and explicitly syncs it later.
func (c *Controller) Adopt(ctx context.Context, host, name string) (*Adoption, error) {
	h, err := c.adoptionHost(host)
	if err != nil {
		return nil, err
	}
	if err := release.ValidateName(name); err != nil {
		return nil, fmt.Errorf("release: %w", ErrNotFound)
	}
	h.Invalidate()
	defer h.Use()()
	rel, err := storage.Latest(h.Store(), name)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("release %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading release %q on %s: %w", name, host, err)
	}
	if rel.Status == release.StatusUninstalled {
		return nil, fmt.Errorf("release %q: %w", name, ErrNotFound)
	}
	if why := c.adoptionBlocked(name, rel); why != "" {
		return nil, fmt.Errorf("%w: %s", ErrAdoptionBlocked, why)
	}
	out := &Adoption{Warnings: []string{"This YAML contains saved user values in plaintext. Review them and encrypt secrets with SOPS before committing. Automation and prune are disabled; commit, run app diff, then sync explicitly."}}
	src := config.AppSource{Values: rel.Config}
	if registry.IsOCI(rel.Chart.Ref) {
		// Preserve a recorded tag/digest, or pin a bare reference to its recorded version.
		src.Chart = rel.Chart.Ref
		if ref, err := registry.ParseReference(rel.Chart.Ref); err == nil && ref.Tag == "" && ref.Digest == "" {
			src.Version = rel.Chart.Version
		}
	} else if path := c.adoptionChartPath(ctx, rel.Chart.Ref); path != "" {
		src.Path = path
	} else {
		src.Path = "charts/" + name
		out.Warnings = append(out.Warnings, "The recorded chart is not available under the root repository. Copy it to "+src.Path+" or replace source with its published OCI reference before syncing.")
	}
	if rel.ProjectDir != h.ProjectDir(name) {
		out.Warnings = append(out.Warnings, fmt.Sprintf("Recorded project directory is %q; this host would use %q. Match the Host projectRoot before syncing to preserve relative bind mounts.", rel.ProjectDir, h.ProjectDir(name)))
	}
	out.Warnings = append(out.Warnings, "Review host environment, chart version and rendered diff; the release record does not retain CLI environment overrides.")
	doc := struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Metadata   config.Metadata `json:"metadata"`
		Spec       config.AppSpec  `json:"spec"`
	}{config.APIVersion, config.KindApplication, config.Metadata{Name: name}, config.AppSpec{Source: src, Destination: config.Destination{Host: host}}}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encoding Application YAML")
	}
	out.YAML = string(data)
	return out, nil
}

// adoptionChartPath only suggests a path when the recorded location maps into
// the current root tree and that tree contains the chart. Never infer a chart
// from its basename, or permit a symlink to escape the repository.
func (c *Controller) adoptionChartPath(ctx context.Context, ref string) string {
	root := c.Root()
	if root.Revision == "" || c.Git == nil {
		return ""
	}
	defer c.useCheckout(root.RepoURL, root.Revision)()
	checkout, err := c.Git.Checkout(ctx, root.RepoURL, root.Revision)
	if err != nil {
		return ""
	}
	checkout, err = filepath.EvalSymlinks(checkout)
	if err != nil {
		return ""
	}
	path := ref
	if filepath.IsAbs(ref) {
		ref, err = filepath.EvalSymlinks(ref)
		if err != nil {
			return ""
		}
		bases := []string{checkout}
		if filepath.IsAbs(root.RepoURL) {
			bases = append(bases, root.RepoURL)
		}
		path = ""
		for _, base := range bases {
			if real, err := filepath.EvalSymlinks(base); err == nil {
				if p, err := filepath.Rel(real, ref); err == nil && confinedPath(p) {
					path = p
					break
				}
			}
		}
	}
	if path == "" || !confinedPath(path) {
		return ""
	}
	real, err := filepath.EvalSymlinks(filepath.Join(checkout, path))
	if err != nil {
		return ""
	}
	p, err := filepath.Rel(checkout, real)
	if err != nil || !confinedPath(p) {
		return ""
	}
	if st, err := os.Stat(filepath.Join(real, "Chart.yaml")); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return filepath.ToSlash(p)
}

func confinedPath(p string) bool {
	return !filepath.IsAbs(p) && p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator)) && !strings.Contains(p, `\`)
}
