// Package features lists every setting composecd reads, from composecd.yaml,
// the objects in Git and compose service labels, with what each one does and
// its default. The web UI shows an object's settings from it, set or not, so
// what composecd can do is visible where it would be configured.
package features

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
)

// Scope is where a setting is written.
type Scope string

const (
	// Server settings are in composecd.yaml.
	Server Scope = "server"
	// Host, Application and ApplicationSet settings are in those objects.
	Host           Scope = "host"
	Application    Scope = "application"
	ApplicationSet Scope = "applicationset"
	// ServiceLabel settings are compose labels on a chart's services.
	ServiceLabel Scope = "label"
)

// Feature is one setting.
type Feature struct {
	Scope Scope
	// Path is the setting's YAML path in its object (or composecd.yaml): keys
	// joined by dots, with [] for the items of a list. A ServiceLabel's is the
	// label.
	Path string
	// Group is the heading it's shown under.
	Group string
	Title string
	// Help says what it does.
	Help string
	// Default is what applies when it isn't set ("" when nothing does).
	Default string
	// Example is a value to set it to, in YAML; for a ServiceLabel, the
	// labels to add.
	Example string
	// Doc is the README section that explains it.
	Doc string
	// Secret values are never shown, only that they are set.
	Secret bool
	// Sensitive values (a map) show only their keys to readonly users.
	Sensitive bool
	// inherit is the value an object that doesn't set Path takes from
	// elsewhere, and where from.
	inherit func(Context) (value, from string, ok bool)
	// resolved says where a value the loader filled in (it can't tell
	// from one written) comes from, when it is that value.
	resolved func(ctx Context, value string) (from string, ok bool)
}

// DocBase is where Doc anchors point.
const DocBase = "https://github.com/trevorswanson/composecd#"

// DocURL is the link to the feature's documentation, or "".
func (f Feature) DocURL() string {
	if f.Doc == "" {
		return ""
	}
	return DocBase + f.Doc
}

// Key is the setting's path inside its list's items, or its whole path.
func (f Feature) Key() string {
	if i := strings.LastIndex(f.Path, "[]."); i >= 0 {
		return f.Path[i+3:]
	}
	return f.Path
}

// Item is true for the fields of a list's items, which are shown with their
// list rather than on their own.
func (f Feature) Item() bool { return strings.Contains(f.Path, "[]") }

// For lists the features of scope, in order.
func For(scope Scope) []Feature {
	var out []Feature
	for _, f := range registry {
		if f.Scope == scope {
			out = append(out, f)
		}
	}
	return out
}

// State is whether a setting is set.
type State string

const (
	Set       State = "set"
	Inherited State = "inherited"
	Default   State = "default"
)

// Setting is a feature as one object has it.
type Setting struct {
	Feature
	State State
	// Value is the setting's value as written; Block is true when it is YAML
	// over several lines. For a setting that isn't set, Value is empty.
	Value string
	Block bool
	// From is where an inherited value comes from.
	From string
	// Fields are a list's item fields.
	Fields []Feature
	// Snippet is YAML that sets it, to copy into the object.
	Snippet string
}

// Group is a heading's settings.
type Group struct {
	Name     string
	Settings []Setting
	// Count is how many settings are set or inherited, of Total.
	Count, Total int
}

// Context is what Explain needs besides the object.
type Context struct {
	// Boot is the server's configuration, which objects inherit from.
	Boot *config.Bootstrap
	// Written is the object's YAML as written, when the object has defaults
	// filled in (composecd.yaml): only what it sets counts as set.
	Written []byte
	// Prefix is the path of the object inside the YAML a snippet goes into,
	// such as spec.template for an ApplicationSet's template.
	Prefix string
	// Mask hides secret values in what is shown; nil shows them as they are.
	Mask func(string) string
	// KeysOnly shows only the keys of Sensitive settings, for readonly users.
	KeysOnly bool
}

// Explain lists each feature of scope as obj (a *config.Host,
// *config.Application, *config.ApplicationSet or *config.Bootstrap) has it,
// by group, in the registry's order.
func Explain(scope Scope, obj interface{}, ctx Context) ([]Group, error) {
	effective, err := tree(obj)
	if err != nil {
		return nil, err
	}
	written := effective
	if ctx.Written != nil {
		if err = yaml.Unmarshal(ctx.Written, &written); err != nil {
			return nil, err
		}
	}
	features := For(scope)
	var groups []Group
	for _, f := range features {
		if f.Item() {
			continue
		}
		s := Setting{Feature: f, State: Default, Snippet: snippet(ctx.Prefix, f)}
		for _, item := range features {
			if item.Item() && strings.HasPrefix(item.Path, f.Path+"[].") {
				s.Fields = append(s.Fields, item)
			}
		}
		if _, ok := lookup(written, f.Path); ok {
			v, _ := lookup(effective, f.Path)
			s.State = Set
			if m, ok := v.(map[string]interface{}); ok && f.Sensitive && ctx.KeysOnly {
				v = keys(m)
			}
			s.Value, s.Block = render(f, v, hasChildren(features, f.Path))
			if f.resolved != nil {
				if from, ok := f.resolved(ctx, s.Value); ok {
					s.State, s.From = Inherited, from
				}
			}
		} else if f.inherit != nil {
			if v, from, ok := f.inherit(ctx); ok {
				s.State, s.Value, s.From = Inherited, v, from
			}
		}
		if ctx.Mask != nil {
			s.Value = ctx.Mask(s.Value)
		}
		if len(groups) == 0 || groups[len(groups)-1].Name != f.Group {
			groups = append(groups, Group{Name: f.Group})
		}
		g := &groups[len(groups)-1]
		g.Settings = append(g.Settings, s)
		g.Total++
		if s.State != Default {
			g.Count++
		}
	}
	return groups, nil
}

// ExplainLabels lists the service labels, with the value each service of
// labels (service name to its compose labels) gives them.
func ExplainLabels(labels map[string]map[string]string) Group {
	g := Group{Name: "Service labels"}
	services := make([]string, 0, len(labels))
	for svc := range labels {
		services = append(services, svc)
	}
	sort.Strings(services)
	for _, f := range For(ServiceLabel) {
		s := Setting{Feature: f, State: Default, Snippet: f.Example}
		var set []string
		for _, svc := range services {
			if v, ok := labels[svc][f.Path]; ok {
				set = append(set, svc+": "+v)
			}
		}
		if set != nil {
			s.State, s.Value = Set, strings.Join(set, "\n")
			s.Block = len(set) > 1
			g.Count++
		}
		g.Settings = append(g.Settings, s)
		g.Total++
	}
	return g
}

// tree is obj as generic YAML.
func tree(obj interface{}) (interface{}, error) {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var out interface{}
	err = yaml.Unmarshal(data, &out)
	return out, err
}

// lookup finds a dotted path (without []) in a generic YAML tree.
func lookup(t interface{}, path string) (interface{}, bool) {
	for _, key := range strings.Split(path, ".") {
		m, ok := t.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if t, ok = m[key]; !ok {
			return nil, false
		}
	}
	return t, true
}

func hasChildren(features []Feature, path string) bool {
	for _, f := range features {
		if strings.HasPrefix(f.Path, path+".") {
			return true
		}
	}
	return false
}

// render shows a set value: "On" for a section whose fields are listed on
// their own, a scalar or list of scalars inline, anything else as YAML.
func render(f Feature, v interface{}, section bool) (string, bool) {
	if f.Secret {
		return "Set (hidden)", false
	}
	if section {
		return "On", false
	}
	v = redact(f.Path, v)
	switch x := v.(type) {
	case nil:
		return "{}", false
	case map[string]interface{}:
		if len(x) == 0 {
			return "{}", false
		}
	case []interface{}:
		if len(x) == 0 {
			return "[]", false
		}
		if s, ok := scalars(x); ok {
			return s, false
		}
	default:
		return fmt.Sprint(x), false
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprint(v), false
	}
	return strings.TrimRight(string(data), "\n"), true
}

// keys are m's keys, sorted, in place of its values.
func keys(m map[string]interface{}) []interface{} {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]interface{}, len(names))
	for i, k := range names {
		out[i] = k + " (value hidden)"
	}
	return out
}

// scalars joins a list of scalars with commas; ok is false for a list of
// anything else.
func scalars(list []interface{}) (string, bool) {
	out := make([]string, len(list))
	for i, item := range list {
		switch item.(type) {
		case map[string]interface{}, []interface{}:
			return "", false
		}
		out[i] = fmt.Sprint(item)
	}
	return strings.Join(out, ", "), true
}

// redact hides the secret fields of a list's items, and the credentials of
// any URL, in v at path.
func redact(path string, v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, item := range x {
			if secret(path + "." + k) {
				out[k] = "(hidden)"
				continue
			}
			out[k] = redact(path+"."+k, item)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, item := range x {
			out[i] = redact(path+"[]", item)
		}
		return out
	case string:
		return stripCredentials(x)
	}
	return v
}

func secret(path string) bool {
	for _, f := range registry {
		if f.Path == path && f.Secret {
			return true
		}
	}
	return false
}

// stripCredentials hides the password of a URL, and the whole user of an
// HTTP URL, where a token may be written as the user name. An SSH user is
// kept: it says who composecd connects as.
func stripCredentials(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	_, password := u.User.Password()
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		u.User = url.User("hidden")
	case password:
		u.User = url.UserPassword(u.User.Username(), "hidden")
	default:
		return s
	}
	return u.String()
}

// snippet is YAML that sets f, nested under prefix.
func snippet(prefix string, f Feature) string {
	if f.Example == "" {
		return ""
	}
	path := f.Path
	if prefix != "" {
		path = prefix + "." + path
	}
	keys := strings.Split(path, ".")
	var b strings.Builder
	for i, key := range keys {
		b.WriteString(strings.Repeat("  ", i) + key + ":")
		if i < len(keys)-1 {
			b.WriteString("\n")
		}
	}
	if !strings.Contains(f.Example, "\n") {
		return b.String() + " " + f.Example
	}
	indent := strings.Repeat("  ", len(keys))
	for _, line := range strings.Split(f.Example, "\n") {
		b.WriteString("\n" + indent + line)
	}
	return b.String()
}
