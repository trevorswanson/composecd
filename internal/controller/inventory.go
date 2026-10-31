package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/release"
	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
)

// LookupApplication is the lookup kind that reads other Applications:
// lookup "composecd/application" "" lists them all (under items), and
// lookup "composecd/application" NAME returns one, or {} if there is none.
const LookupApplication = "composecd/application"

// rememberManifest records the compose manifest lookup reports for app: the
// desired one it last rendered from Git, or, until one renders, the deployed
// one ("" when nothing is deployed).
func (c *Controller) rememberManifest(name, manifest string, desired bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.apps[name]
	if !ok {
		s = &appState{}
		c.apps[name] = s
	}
	if desired || !s.known {
		s.manifest, s.known = manifest, true
	}
}

// deployedManifest is the manifest of hist's deployed revision, or "".
func deployedManifest(hist []*release.Release) string {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Status == release.StatusDeployed {
			return hist[i].Manifest
		}
	}
	return ""
}

// markReader records whether app's last render read other Applications, so
// RefreshAll can reconcile it after them.
func (c *Controller) markReader(name string, reads bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.apps[name]; ok {
		s.reader = reads
	} else if reads {
		c.apps[name] = &appState{reader: true}
	}
}

func (c *Controller) isReader(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.apps[name]
	return ok && s.reader
}

// appLookup is the lookup template function for app's renders. One snapshot of
// the other Applications serves every call, so the render assess compares and
// the one a sync deploys see the same data. reads reports whether the chart
// asked for it.
type appLookup struct {
	c    *Controller
	ctx  context.Context
	cfg  *action.Configuration
	app  *config.Application
	once sync.Once
	apps map[string]interface{}
	err  error
	used bool
	mu   sync.Mutex
}

func (c *Controller) newLookup(ctx context.Context, cfg *action.Configuration, app *config.Application) *appLookup {
	return &appLookup{c: c, ctx: ctx, cfg: cfg, app: app}
}

// Func is the function for composetpl's Lookup options.
func (l *appLookup) Func(kind, name string) (map[string]interface{}, error) {
	if kind != LookupApplication {
		return l.cfg.Compose.Inspect(l.ctx, kind, name)
	}
	l.mu.Lock()
	l.used = true
	l.mu.Unlock()
	l.once.Do(func() { l.apps, l.err = l.c.inventory(l.app) })
	if l.err != nil {
		return nil, fmt.Errorf("lookup %s: %w", LookupApplication, l.err)
	}
	if name == "" {
		items := make([]interface{}, 0, len(l.apps))
		for _, n := range sortedKeys(l.apps) {
			items = append(items, l.apps[n])
		}
		return map[string]interface{}{"items": items}, nil
	}
	if a, ok := l.apps[name].(map[string]interface{}); ok {
		return a, nil
	}
	return map[string]interface{}{}, nil
}

func (l *appLookup) reads() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}

// inventory describes every other Application in Git as reader may see it: identity
// and service labels always, and environment, configs and secrets as reader's
// permissions grant. It fails rather than leave out an Application that hasn't
// been reconciled yet, so a monitoring chart never drops checks it can't see.
func (c *Controller) inventory(reader *config.Application) (map[string]interface{}, error) {
	type entry struct {
		app      *config.Application
		manifest string
	}
	var entries []entry
	var unknown []string
	c.mu.RLock()
	if c.objects != nil {
		for _, a := range c.objects.Applications {
			if a.Metadata.Name == reader.Metadata.Name {
				// Its own manifest holds what it read last time, so including
				// it would change every render.
				continue
			}
			if s, ok := c.apps[a.Metadata.Name]; ok && s.known {
				entries = append(entries, entry{a, s.manifest})
			} else {
				unknown = append(unknown, a.Metadata.Name)
			}
		}
	}
	c.mu.RUnlock()
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("not yet reconciled: %s", strings.Join(unknown, ", "))
	}
	grants := reader.Spec.Permissions.Grants()
	out := make(map[string]interface{}, len(entries))
	for _, e := range entries {
		item, err := inventoryItem(e.app, e.manifest, grants, c.masker(e.app.Metadata.Name).Text)
		if err != nil {
			return nil, fmt.Errorf("application %q: %w", e.app.Metadata.Name, err)
		}
		out[e.app.Metadata.Name] = item
	}
	return out, nil
}

// inventoryItem describes app from its compose manifest. Decrypted values are
// masked, by mask, everywhere except under a secrets grant.
func inventoryItem(app *config.Application, manifest string, grants map[string][]string, mask func(string) string) (map[string]interface{}, error) {
	name := app.Metadata.Name
	var project struct {
		Services map[string]map[string]interface{} `yaml:"services"`
		Configs  map[string]map[string]interface{} `yaml:"configs"`
		Secrets  map[string]map[string]interface{} `yaml:"secrets"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &project); err != nil {
		return nil, fmt.Errorf("reading its manifest: %w", err)
	}
	labels := map[string]interface{}{}
	for k, v := range app.Metadata.Labels {
		labels[k] = v
	}
	services := map[string]interface{}{}
	for svcName, svc := range project.Services {
		s := map[string]interface{}{
			"labels": maskMap(mask, keyValues(svc["labels"])),
		}
		if v, ok := svc["container_name"].(string); ok {
			s["containerName"] = v
		}
		if v, ok := svc["image"].(string); ok {
			s["image"] = mask(v)
		}
		env := keyValues(svc["environment"])
		switch {
		case config.Allows(grants["secrets"], name, svcName):
			s["environment"] = env
			s["secrets"] = mounts(svc["secrets"], project.Secrets, func(def map[string]interface{}) interface{} { return def })
		case config.Allows(grants["env"], name, svcName):
			s["environment"] = maskMap(mask, env)
		}
		if config.Allows(grants["configs"], name, svcName) {
			s["configs"] = mounts(svc["configs"], project.Configs, func(def map[string]interface{}) interface{} {
				if content, ok := def["content"].(string); ok {
					return mask(content)
				}
				return nil
			})
		}
		services[svcName] = s
	}
	return map[string]interface{}{
		"name":     name,
		"host":     app.Spec.Destination.Host,
		"labels":   labels,
		"services": services,
	}, nil
}

// keyValues reads a compose map-or-list field (labels, environment) as a map.
// A list entry without "=" maps to "".
func keyValues(v interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	switch v := v.(type) {
	case map[string]interface{}:
		for k, val := range v {
			if val == nil {
				out[k] = ""
			} else {
				out[k] = fmt.Sprint(val)
			}
		}
	case []interface{}:
		for _, e := range v {
			k, val, _ := strings.Cut(fmt.Sprint(e), "=")
			out[k] = val
		}
	}
	return out
}

func maskMap(mask func(string) string, in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = mask(v.(string))
	}
	return out
}

// mounts maps each config or secret a service mounts, by its target, to what
// value makes of its top-level definition. A short-form mount targets its
// source name, as compose does.
func mounts(refs interface{}, defs map[string]map[string]interface{}, value func(map[string]interface{}) interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	list, _ := refs.([]interface{})
	for _, r := range list {
		var source, target string
		switch r := r.(type) {
		case string:
			source, target = r, r
		case map[string]interface{}:
			source, _ = r["source"].(string)
			target, _ = r["target"].(string)
			if target == "" {
				target = source
			}
		}
		if def, ok := defs[source]; ok {
			out[target] = value(def)
		}
	}
	return out
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
