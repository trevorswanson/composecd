// Package source turns an Application and a checkout of its repository into
// what composetpl renders: a chart reference, user values and an environment.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/chart"
	"github.com/trevorswanson/composetpl/pkg/registry"
	"github.com/trevorswanson/composetpl/pkg/values"
	"sigs.k8s.io/yaml"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/infisical"
	"github.com/trevorswanson/composecd/internal/masking"
	"github.com/trevorswanson/composecd/internal/sops"
)

// Prepared is an Application ready to render.
type Prepared struct {
	// ChartRef is a chart directory or an oci:// reference.
	ChartRef string
	// Version is a semver constraint for an oci:// ChartRef.
	Version string
	// Values are the user values: ValueFiles, then the inline values.
	Values map[string]interface{}
	// Env is the host's env overlaid by the app's, as sorted KEY=VALUE entries.
	Env []string
	// Revision is the commit the chart was read from ("" for an oci:// chart).
	Revision string
	// ValuesRevision is the Git commit supplying an OCI chart's user values.
	ValuesRevision string
	// Root is the checkout the chart and values were read from, if any.
	Root string `json:"-"`
	// Secrets are decrypted leaf values used for output masking, never persisted.
	Secrets []string `json:"-"`
	// SecretPaths locate the encrypted keys in user values. They come from the
	// encrypted files' structure, so they are known even when decryption fails,
	// and let older values at the same keys be found in release history.
	SecretPaths [][]string `json:"-"`
	// Stubbed says which secrets PrepareOffline stood in for, as "valueFiles
	// secrets.sops.yaml" or "source.infisical".
	Stubbed []string `json:"-"`
}

// Placeholder is what PrepareOffline puts in place of each encrypted value.
const Placeholder = "composecd-validate-placeholder"

// Lookup backs the env and expandenv template functions with Env only, so a
// render doesn't depend on the controller's own environment.
func (p *Prepared) Lookup(key string) (string, bool) {
	prefix := key + "="
	for _, e := range p.Env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):], true
		}
	}
	return "", false
}

// depLocks serializes vendoring per chart directory: checkouts are shared.
var depLocks sync.Map

// Prepare resolves app's source. checkout is the root of the app's repository at
// revision. For OCI charts it is the valuesFrom repository root; without
// valuesFrom, checkout and revision are ignored. If the chart has a Chart.lock,
// its dependencies are vendored into the checkout first. When a values file
// can't be decrypted, the error comes with a Prepared holding only SecretPaths.
// inf reads the app's source.infisical secrets; it is nil when the server has
// no infisical configuration.
func Prepare(ctx context.Context, app *config.Application, host *config.Host, checkout, revision string, reg *registry.Client, ageKeyFile string, inf ...*infisical.Client) (*Prepared, error) {
	var c *infisical.Client
	if len(inf) > 0 {
		c = inf[0]
	}
	return prepare(ctx, app, host, checkout, revision, reg, ageKeyFile, c, false)
}

// PrepareOffline is Prepare for validating a checkout without its secrets.
// Without ageKeyFile, each value of an encrypted values file is Placeholder,
// and source.infisical contributes no values, so charts render as long as they
// don't depend on what the secrets say. Stubbed lists what was left out.
func PrepareOffline(ctx context.Context, app *config.Application, host *config.Host, checkout, revision string, reg *registry.Client, ageKeyFile string) (*Prepared, error) {
	return prepare(ctx, app, host, checkout, revision, reg, ageKeyFile, nil, true)
}

func prepare(ctx context.Context, app *config.Application, host *config.Host, checkout, revision string, reg *registry.Client, ageKeyFile string, inf *infisical.Client, offline bool) (*Prepared, error) {
	s := app.Spec.Source
	p := &Prepared{Env: mergeEnv(host.Spec.Env, s.Env)}
	vals := map[string]interface{}{}
	var root, dir string
	if s.IsOCI() {
		p.ChartRef, p.Version = s.Chart, s.Version
		if len(s.ValueFiles) > 0 && s.ValuesFrom == nil {
			return nil, errors.New("OCI valueFiles require valuesFrom")
		}
		if s.ValuesFrom != nil {
			var err error
			root, err = filepath.EvalSymlinks(checkout)
			if err != nil {
				return nil, err
			}
			dir = root
			p.ValuesRevision = revision
		}
	} else {
		var err error
		root, err = filepath.EvalSymlinks(checkout)
		if err != nil {
			return nil, err
		}
		dir, err = inside(root, filepath.Join(root, filepath.FromSlash(s.Path)))
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", s.Path, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
			return nil, fmt.Errorf("path %q at %s is not a chart: no Chart.yaml", s.Path, short(revision))
		}
		p.ChartRef, p.Revision = dir, revision
	}
	p.Root = root
	// After a decryption failure, keep reading the remaining files' SOPS key
	// paths: the caller masks older values at those keys from release history.
	var decryptErr error
	for _, vf := range s.ValueFiles {
		f, err := inside(root, filepath.Join(dir, filepath.FromSlash(vf)))
		if err != nil {
			return nil, fmt.Errorf("valueFiles %q: %w", vf, err)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, relativeErr(err, root)
		}
		encrypted, err := sops.Encrypted(vf, data)
		if err != nil {
			return nil, err
		}
		if encrypted {
			paths := sops.Paths(data)
			p.SecretPaths = append(p.SecretPaths, paths...)
			if decryptErr != nil {
				continue
			}
			if offline && ageKeyFile == "" {
				p.Stubbed = append(p.Stubbed, fmt.Sprintf("valueFiles %q", vf))
				vals = values.Merge(vals, placeholders(paths))
				continue
			}
			data, err = sops.Decrypt(ctx, f, ageKeyFile)
			if err != nil {
				decryptErr = fmt.Errorf("valueFiles %q: %w", vf, err)
				continue
			}
		}
		if decryptErr != nil {
			continue
		}
		cur := map[string]interface{}{}
		if err := yaml.Unmarshal(data, &cur); err != nil {
			return nil, fmt.Errorf("valueFiles %q: invalid values YAML", vf)
		}
		if encrypted {
			p.Secrets = append(p.Secrets, masking.Leaves(cur)...)
		}
		vals = values.Merge(vals, cur)
	}
	if decryptErr != nil {
		return &Prepared{SecretPaths: p.SecretPaths}, decryptErr
	}
	if is := s.Infisical; is != nil && offline {
		p.Stubbed = append(p.Stubbed, "source.infisical")
	} else if is != nil {
		if inf == nil {
			return nil, errors.New("source.infisical requires an infisical section in composecd.yaml")
		}
		secrets, err := inf.Secrets(ctx, is)
		if err != nil {
			return nil, err
		}
		cur, paths, err := nest(is.Into, secrets)
		if err != nil {
			return nil, err
		}
		p.SecretPaths = append(p.SecretPaths, paths...)
		for _, x := range secrets {
			p.Secrets = append(p.Secrets, masking.Leaves(x.Value)...)
		}
		vals = values.Merge(vals, cur)
	}
	if !s.IsOCI() {
		if err := vendor(ctx, dir, reg); err != nil {
			return nil, err
		}
	}
	inline, err := normalize(s.Values)
	if err != nil {
		return nil, err
	}
	p.Values = values.Merge(vals, inline)
	return p, nil
}

// placeholders builds values holding Placeholder at each of paths.
func placeholders(paths [][]string) map[string]interface{} {
	root := map[string]interface{}{}
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		cur := root
		for _, seg := range path[:len(path)-1] {
			next, ok := cur[seg].(map[string]interface{})
			if !ok {
				next = map[string]interface{}{}
				cur[seg] = next
			}
			cur = next
		}
		cur[path[len(path)-1]] = Placeholder
	}
	return root
}

// nest places secrets under the dotted path into, then under their subfolders,
// and returns the value paths they occupy. A later secret with the same key and
// folder replaces an earlier one. A secret and a folder of the same name at one
// level can't both be values, so that is an error.
func nest(into string, secrets []infisical.Secret) (map[string]interface{}, [][]string, error) {
	var prefix []string
	if into != "" {
		prefix = strings.Split(into, ".")
	}
	root := map[string]interface{}{}
	var paths [][]string
	for _, x := range secrets {
		full := append(append([]string{}, prefix...), x.Folder...)
		cur := root
		for _, seg := range full {
			switch next := cur[seg].(type) {
			case nil:
				m := map[string]interface{}{}
				cur[seg] = m
				cur = m
			case map[string]interface{}:
				cur = next
			default:
				return nil, nil, fmt.Errorf("infisical: folder %q collides with a secret of the same name", strings.Join(full, "/"))
			}
		}
		switch cur[x.Key].(type) {
		case nil:
			paths = append(paths, append(full, x.Key))
		case map[string]interface{}:
			return nil, nil, fmt.Errorf("infisical: secret %q collides with a folder of the same name", x.Key)
		}
		cur[x.Key] = x.Value
	}
	return root, paths, nil
}

// inside resolves symlinks in p and checks that it stays under root, so a
// repository can't point composecd at files elsewhere on the controller.
func inside(root, p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("not found in the repository")
		}
		return "", err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("resolves outside the repository")
	}
	return real, nil
}

// vendor runs `composetpl dependency build` when the chart has a Chart.lock.
func vendor(ctx context.Context, dir string, reg *registry.Client) error {
	if _, err := os.Stat(filepath.Join(dir, chart.LockFile)); err != nil {
		return nil
	}
	l, _ := depLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	d := &action.Dependencies{Registry: reg, Log: io.Discard}
	if _, err := d.Build(ctx, dir); err != nil {
		return fmt.Errorf("building chart dependencies: %w", err)
	}
	return nil
}

// normalize round-trips inline values through JSON so they have the same types
// (float64 numbers, map[string]interface{}) as values read from files.
func normalize(v map[string]interface{}) (map[string]interface{}, error) {
	if len(v) == 0 {
		return map[string]interface{}{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("values: %w", err)
	}
	out := map[string]interface{}{}
	return out, json.Unmarshal(data, &out)
}

// Env is the environment of app's docker calls on host: the host's env
// overlaid by the app's, as sorted KEY=VALUE entries.
func Env(host *config.Host, app *config.Application) []string {
	return mergeEnv(host.Spec.Env, app.Spec.Source.Env)
}

// mergeEnv overlays app onto host as sorted KEY=VALUE entries.
func mergeEnv(host, app map[string]string) []string {
	m := map[string]string{}
	for k, v := range host {
		m[k] = v
	}
	for k, v := range app {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// RelativeErr shortens checkout paths in err (from rendering, say) to
// repository-relative ones, so errors name files as they are in Git.
func (p *Prepared) RelativeErr(err error) error {
	if err == nil || p.Root == "" {
		return err
	}
	return relativeErr(err, p.Root)
}

// relativeErr shortens checkout paths in err to repository-relative ones.
func relativeErr(err error, root string) error {
	return errors.New(strings.ReplaceAll(err.Error(), root+string(filepath.Separator), ""))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
