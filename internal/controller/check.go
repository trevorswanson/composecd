package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/registry"

	"github.com/trevorswanson/composecd/internal/appset"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/masking"
	"github.com/trevorswanson/composecd/internal/source"
)

// CheckOptions configure Check.
type CheckOptions struct {
	// RepoURL is the URL the checkout around the objects is fetched from.
	// Applications and gitDirectories generators that name it, or no
	// repository, read the checkout; others are skipped.
	RepoURL string
	// AgeKeyFile decrypts SOPS values; without it they are placeholders.
	AgeKeyFile string
	// Registry pulls OCI charts and dependencies; nil uses a default client.
	Registry *registry.Client
	// Compose, when set, checks each manifest with `docker compose config`.
	Compose bool
}

// CheckResult is one Application's offline render.
type CheckResult struct {
	Name   string
	Host   string
	Origin string
	// Manifest is the rendered compose project, hooks removed.
	Manifest string
	// Skipped says why the Application wasn't rendered.
	Skipped string
	// Stubbed lists the secrets that rendered as placeholders or empty.
	Stubbed []string
	// Notes are caveats about the render, such as lookups that couldn't see
	// every Application.
	Notes []string
	Err   error
}

// CheckSet is an ApplicationSet's expansion.
type CheckSet struct {
	Name         string
	Applications []string
	Skipped      string
	Warnings     []string
	Err          error
}

// errOtherRepo marks a repository that isn't the checkout being checked.
var errOtherRepo = errors.New("not this checkout")

// Check expands objs' ApplicationSets and renders every Application from the
// Git checkout around dir, the way the server would, without contacting hosts
// or reading secrets it has no key for. lookup "composecd/application" sees
// the other Applications' renders; lookups of engine objects return {}. It
// returns an error only when the expanded objects fail validation, or
// o.Compose finds no docker compose; render failures are in the results.
func Check(ctx context.Context, dir string, objs *config.Objects, o CheckOptions) ([]CheckResult, []CheckSet, error) {
	root, err := checkoutRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	if o.Compose {
		if _, err := (&compose.CLI{}).Version(ctx); err != nil {
			return nil, nil, err
		}
	}
	local := func(url string) bool { return url == "" || sameRepo(url, o.RepoURL) }
	objs.Resolve(config.Root{RepoURL: o.RepoURL})

	expanded := *objs
	expanded.Applications = append([]*config.Application{}, objs.Applications...)
	checkout := func(_ context.Context, url, _ string) (string, func(), error) {
		if !local(url) {
			return "", nil, fmt.Errorf("generator repository %s: %w", url, errOtherRepo)
		}
		return root, func() {}, nil
	}
	sets := make([]CheckSet, 0, len(objs.ApplicationSets))
	for _, set := range objs.ApplicationSets {
		apps, skipped, err := appset.Generate(ctx, set, objs.Hosts, checkout)
		cs := CheckSet{Name: set.Metadata.Name}
		switch {
		case errors.Is(err, errOtherRepo):
			cs.Skipped = err.Error()
		case err != nil:
			cs.Err = err
		default:
			cs.Warnings = setWarnings(len(apps), skipped)
		}
		for _, a := range apps {
			cs.Applications = append(cs.Applications, a.Metadata.Name)
		}
		expanded.Applications = append(expanded.Applications, apps...)
		sets = append(sets, cs)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
	expanded.Resolve(config.Root{RepoURL: o.RepoURL})
	if err := expanded.Validate(); err != nil {
		return nil, sets, err
	}

	results := make([]*CheckResult, 0, len(expanded.Applications))
	byName := map[string]*CheckResult{}
	preps := map[string]*source.Prepared{}
	for _, a := range expanded.Applications {
		r := &CheckResult{Name: a.Metadata.Name, Host: a.Spec.Destination.Host, Origin: a.Origin}
		results = append(results, r)
		byName[r.Name] = r
		if url, _ := a.Spec.Source.GitRepository(); !local(url) {
			r.Skipped = fmt.Sprintf("its source repository %s isn't this checkout", url)
			continue
		}
		prep, err := source.PrepareOffline(ctx, a, expanded.Host(a.Spec.Destination.Host), root, "", o.Registry, o.AgeKeyFile)
		if err != nil {
			r.Err = err
			continue
		}
		r.Stubbed = prep.Stubbed
		preps[r.Name] = prep
	}

	// Render until every Application that reads others has seen them all. When
	// readers wait on each other, the rest render without the ones still
	// waiting, as the server's first reconcile would.
	pending := make([]*config.Application, 0, len(preps))
	for _, a := range expanded.Applications {
		if preps[a.Metadata.Name] != nil {
			pending = append(pending, a)
		}
	}
	rendered := map[string]bool{}
	for _, r := range results {
		if r.Skipped != "" || r.Err != nil {
			rendered[r.Name] = true
		}
	}
	lenient := false
	for len(pending) > 0 {
		var waiting []*config.Application
		for _, a := range pending {
			r := byName[a.Metadata.Name]
			prep := preps[r.Name]
			l := &checkLookup{reader: a, apps: &expanded, results: byName, preps: preps, rendered: rendered, lenient: lenient}
			manifest, _, err := renderManifest(action.RenderOptions{
				ChartPath: prep.ChartRef, Version: prep.Version, Registry: o.Registry, Context: ctx,
				ReleaseName: r.Name, Revision: 1, UserValues: prep.Values, Env: prep.Lookup, Lookup: l.Func,
			})
			if l.waiting != nil && !lenient {
				waiting = append(waiting, a)
				continue
			}
			if len(l.missing) > 0 {
				r.Notes = append(r.Notes, "lookup couldn't see "+strings.Join(l.missing, ", ")+", which read Applications too")
			}
			rendered[r.Name] = true
			if err != nil {
				r.Err = prep.RelativeErr(err)
				continue
			}
			r.Manifest = manifest
			if o.Compose {
				cli := &compose.CLI{Env: prep.Env}
				if err := cli.Config(ctx, compose.Project{Name: r.Name, Manifest: []byte(manifest)}); err != nil {
					// composetpl names the failed command after its first argument, -p.
					r.Err = fmt.Errorf("docker compose config: %s", strings.TrimPrefix(err.Error(), "docker compose -p: "))
				}
			}
		}
		lenient = len(waiting) == len(pending)
		pending = waiting
	}

	out := make([]CheckResult, len(results))
	for i, r := range results {
		out[i] = *r
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, sets, nil
}

// checkLookup is lookup for an offline render. waiting is set when the reader
// asked for Applications that haven't rendered yet; lenient leaves them out,
// in missing, instead.
type checkLookup struct {
	reader   *config.Application
	apps     *config.Objects
	results  map[string]*CheckResult
	preps    map[string]*source.Prepared
	rendered map[string]bool
	lenient  bool
	waiting  error
	missing  []string
}

var errWaiting = errors.New("waiting for other Applications to render")

func (l *checkLookup) Func(kind, name string) (map[string]interface{}, error) {
	if kind != LookupApplication {
		return map[string]interface{}{}, nil
	}
	grants := l.reader.Spec.Permissions.Grants()
	items := map[string]interface{}{}
	for _, a := range l.apps.Applications {
		n := a.Metadata.Name
		if n == l.reader.Metadata.Name || (name != "" && n != name) {
			continue
		}
		if !l.rendered[n] {
			if !l.lenient {
				l.waiting = errWaiting
				return nil, errWaiting
			}
			if !slices.Contains(l.missing, n) {
				l.missing = append(l.missing, n)
			}
			continue
		}
		r := l.results[n]
		if r.Skipped != "" {
			continue
		}
		var secrets []string
		if p := l.preps[n]; p != nil {
			secrets = p.Secrets
		}
		item, err := inventoryItem(a, r.Manifest, grants, masking.New(secrets).Text)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: application %q: %w", LookupApplication, n, err)
		}
		items[n] = item
	}
	if name != "" {
		if a, ok := items[name].(map[string]interface{}); ok {
			return a, nil
		}
		return map[string]interface{}{}, nil
	}
	list := make([]interface{}, 0, len(items))
	for _, n := range sortedKeys(items) {
		list = append(list, items[n])
	}
	return map[string]interface{}{"items": list}, nil
}

// checkoutRoot is the Git work tree around dir, or dir itself outside one.
func checkoutRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for p := abs; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p, nil
		}
		if filepath.Dir(p) == p {
			return abs, nil
		}
	}
}

// sameRepo compares repository URLs, ignoring a trailing .git or slash, the
// scheme, credentials and host case, so a CI clone URL matches the one in
// composecd.yaml.
func sameRepo(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	norm := func(u string) string {
		u = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git")
		if i := strings.Index(u, "://"); i >= 0 {
			u = u[i+3:]
		} else if c, s := strings.Index(u, ":"), strings.Index(u, "/"); c >= 0 && (s < 0 || c < s) {
			u = u[:c] + "/" + u[c+1:] // git@host:owner/repo
		}
		host, path, _ := strings.Cut(u, "/")
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		host, _, _ = strings.Cut(host, ":")
		return strings.ToLower(host) + "/" + path
	}
	return norm(a) == norm(b)
}
