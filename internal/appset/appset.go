// Package appset expands independent Git-directory and host generators into
// Applications. It never reads credentials or the process environment.
package appset

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"github.com/trevorswanson/composecd/internal/config"
)

type Status struct {
	Name         string   `json:"name"`
	Origin       string   `json:"origin,omitempty"`
	Generated    int      `json:"generated"`
	Previous     int      `json:"previous,omitempty"`
	Applications []string `json:"applications"`
	Ready        bool     `json:"ready"`
	Error        string   `json:"error,omitempty"`
}

// Checkout provides a protected, resolved checkout and its release function.
type Checkout func(context.Context, string, string) (string, func(), error)

// Generate expands each generator separately. Any failure discards this set's
// entire output, preventing partial results from deleting previously known apps.
func Generate(ctx context.Context, set *config.ApplicationSet, hosts []*config.Host, checkout Checkout) ([]*config.Application, error) {
	var apps []*config.Application
	for _, g := range set.Spec.Generators {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case g.GitDirectories != nil && g.Hosts == nil:
			d := g.GitDirectories
			dir, done, err := checkout(ctx, d.RepoURL, d.TargetRevision)
			if err != nil {
				return nil, err
			}
			generated, err := func() ([]*config.Application, error) {
				defer done()
				dirs, err := directories(ctx, dir, d)
				if err != nil {
					return nil, err
				}
				var out []*config.Application
				for _, p := range dirs {
					params := map[string]interface{}{"path": map[string]interface{}{"path": p, "basename": path.Base(p), "segments": strings.Split(p, "/")}}
					a, err := render(set, params, p)
					if err != nil {
						return nil, err
					}
					if !a.Spec.Source.IsOCI() && a.Spec.Source.RepoURL == "" {
						a.Spec.Source.RepoURL = d.RepoURL
						if a.Spec.Source.TargetRevision == "" {
							a.Spec.Source.TargetRevision = d.TargetRevision
						}
					}
					out = append(out, a)
				}
				return out, nil
			}()
			if err != nil {
				return nil, err
			}
			apps = append(apps, generated...)
		case g.Hosts != nil && g.GitDirectories == nil:
			for _, h := range hosts {
				matches := true
				for k, want := range g.Hosts.Selector {
					got, ok := h.Metadata.Labels[k]
					if !ok || got != want {
						matches = false
						break
					}
				}
				if !matches {
					continue
				}
				params := map[string]interface{}{"host": map[string]interface{}{"name": h.Metadata.Name, "labels": h.Metadata.Labels}}
				a, err := render(set, params, "host "+h.Metadata.Name)
				if err != nil {
					return nil, err
				}
				apps = append(apps, a)
			}
		default:
			return nil, fmt.Errorf("choose exactly one supported generator")
		}
	}
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].Metadata.Name < apps[j].Metadata.Name })
	return apps, nil
}

func directories(ctx context.Context, root string, g *config.GitDirectories) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !matches(g.Directories, rel) || matches(g.Exclude, rel) {
			return nil
		}
		st, err := os.Lstat(filepath.Join(p, "Chart.yaml"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s/Chart.yaml must be a regular repository file", rel)
		}
		dirs = append(dirs, rel)
		return nil
	})
	sort.Strings(dirs)
	return dirs, err
}
func matches(globs []string, s string) bool {
	for _, g := range globs {
		ok, _ := path.Match(g, s)
		if ok {
			return true
		}
	}
	return false
}

// Only deterministic string helpers are exposed. Sprig's environment, file,
// network, crypto, date and random helpers are deliberately unavailable.
var stringFuncs = func() template.FuncMap {
	all := sprig.TxtFuncMap()
	out := template.FuncMap{}
	for _, name := range strings.Fields("trim trimAll trimPrefix trimSuffix upper lower title untitle repeat substr replace nospace trunc abbrev abbrevboth initials contains hasPrefix hasSuffix quote squote cat indent nindent wrap wrapWith snakecase camelcase kebabcase swapcase plural splitList join") {
		out[name] = all[name]
	}
	return out
}()

func render(set *config.ApplicationSet, params map[string]interface{}, origin string) (*config.Application, error) {
	data, err := json.Marshal(set.Spec.Template)
	if err != nil {
		return nil, fmt.Errorf("encoding Application template: %w", err)
	}
	var tree interface{}
	if err = json.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	var walk func(interface{}, string) (interface{}, error)
	walk = func(v interface{}, field string) (interface{}, error) {
		switch v := v.(type) {
		case string:
			t, err := template.New(field).Funcs(stringFuncs).Option("missingkey=error").Parse(v)
			if err != nil {
				return nil, err
			}
			var b bytes.Buffer
			if err = t.Execute(&b, params); err != nil {
				return nil, err
			}
			return b.String(), nil
		case map[string]interface{}:
			out := make(map[string]interface{}, len(v))
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				renderedKey, err := walk(k, field+" key")
				if err != nil {
					return nil, err
				}
				key := renderedKey.(string)
				if _, exists := out[key]; exists {
					return nil, fmt.Errorf("%s: duplicate rendered map key %q", field, key)
				}
				x, err := walk(v[k], field+"."+k)
				if err != nil {
					return nil, err
				}
				out[key] = x
			}
			return out, nil
		case []interface{}:
			for i, x := range v {
				value, err := walk(x, fmt.Sprintf("%s[%d]", field, i))
				if err != nil {
					return nil, err
				}
				v[i] = value
			}
		}
		return v, nil
	}
	tree, err = walk(tree, "template")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	data, err = json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	var a config.Application
	if err = json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("%s: decoding generated Application: %w", origin, err)
	}
	if a.Metadata.Labels == nil {
		a.Metadata.Labels = map[string]string{}
	}
	a.Metadata.Labels[config.LabelApplicationSet] = set.Metadata.Name
	a.Origin = fmt.Sprintf("applicationset %s (%s)", set.Metadata.Name, origin)
	return &a, nil
}
