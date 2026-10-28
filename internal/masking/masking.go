// Package masking redacts known secret values from human-readable output.
package masking

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const Replacement = "••••••"

// Leaves returns scalar values of at least four characters. Individual lines
// of multiline secrets are included because diffs and logs add line prefixes.
func Leaves(value interface{}) []string {
	set := map[string]bool{}
	var visit func(interface{})
	visit = func(v interface{}) {
		switch v := v.(type) {
		case map[string]interface{}:
			for _, x := range v {
				visit(x)
			}
		case map[interface{}]interface{}:
			for _, x := range v {
				visit(x)
			}
		case map[string]string:
			for _, x := range v {
				visit(x)
			}
		case []interface{}:
			for _, x := range v {
				visit(x)
			}
		case nil:
		default:
			s := fmt.Sprint(v)
			if len([]rune(s)) >= 4 {
				set[s] = true
			}
			for _, line := range strings.Split(s, "\n") {
				if len([]rune(line)) >= 4 {
					set[line] = true
				}
			}
		}
	}
	visit(value)
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Masker uses a single replacement pass, longest secrets first, so replacements
// never get interpreted as another secret and overlapping secrets remain hidden.
type Masker struct{ r *strings.Replacer }

func New(secrets []string) *Masker {
	set := map[string]bool{}
	var sorted []string
	for _, s := range secrets {
		if s != "" && !set[s] {
			set[s] = true
			sorted = append(sorted, s)
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i]) == len(sorted[j]) {
			return sorted[i] < sorted[j]
		}
		return len(sorted[i]) > len(sorted[j])
	})
	pairs := make([]string, 0, 2*len(sorted))
	for _, s := range sorted {
		pairs = append(pairs, s, Replacement)
	}
	return &Masker{r: strings.NewReplacer(pairs...)}
}
func (m *Masker) Text(s string) string { return m.r.Replace(s) }

// Redact makes an independent copy of a JSON-shaped value, masking string
// values, including nested maps and arrays. Keys and typed numeric fields keep
// their meaning; escaped quotes and newlines are handled by the JSON encoder.
func (m *Masker) Redact(in, out interface{}) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var tree interface{}
	if err = json.Unmarshal(raw, &tree); err != nil {
		return err
	}
	var walk func(interface{}) interface{}
	walk = func(v interface{}) interface{} {
		switch v := v.(type) {
		case string:
			return m.Text(v)
		case map[string]interface{}:
			for k, x := range v {
				v[k] = walk(x)
			}
		case []interface{}:
			for i, x := range v {
				v[i] = walk(x)
			}
		}
		return v
	}
	raw, err = json.Marshal(walk(tree))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// Values also masks numeric/boolean leaves in untyped user values. Typed
// controller fields such as revision numbers and exit codes are left numeric.
func (m *Masker) Values(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	var walk func(interface{}) interface{}
	walk = func(v interface{}) interface{} {
		switch v := v.(type) {
		case map[string]interface{}:
			return m.Values(v)
		case []interface{}:
			out := make([]interface{}, len(v))
			for i, x := range v {
				out[i] = walk(x)
			}
			return out
		case nil:
			return nil
		case string:
			return m.Text(v)
		default:
			if s := m.Text(fmt.Sprint(v)); s != fmt.Sprint(v) {
				return s
			}
			return v
		}
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = walk(v)
	}
	return out
}
