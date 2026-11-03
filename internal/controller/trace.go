package controller

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/chart"
	"github.com/trevorswanson/composetpl/pkg/registry"
	"github.com/trevorswanson/composetpl/pkg/values"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/images"
	"github.com/trevorswanson/composecd/internal/yamledit"
)

// KeyAppVersion is the tag-key that names the chart's appVersion, which
// templates read as .Chart.AppVersion.
const KeyAppVersion = "Chart.appVersion"

// maxTraceLeaves bounds the renders tracing one service costs.
const maxTraceLeaves = 40

// tagSource is a values key (or the chart's appVersion) that sets a service's
// tag, found by rendering with the key's value replaced by a sentinel.
type tagSource struct {
	// Key is a dotted values key, or KeyAppVersion.
	Key string
	// prefix and suffix are what the render puts around the key's value in
	// the image: nginx: and "" for a key holding the tag.
	prefix, suffix string
	// feeds are the services whose images the key sets, sorted.
	feeds []string
}

// valueFor is what the key must hold for the image to become want, if the
// key covers the part of the image that differs.
func (t tagSource) valueFor(want string) (string, bool) {
	if len(want) <= len(t.prefix)+len(t.suffix) || !strings.HasPrefix(want, t.prefix) || !strings.HasSuffix(want, t.suffix) {
		return "", false
	}
	return want[len(t.prefix) : len(want)-len(t.suffix)], true
}

// forImage keeps the sources that can make the image want: those covering
// the part of the image that changes.
func forImage(srcs []tagSource, want string) []tagSource {
	var out []tagSource
	for _, t := range srcs {
		if _, ok := t.valueFor(want); ok {
			out = append(out, t)
		}
	}
	return out
}

// tracer renders one Application offline, as validate --render does, with
// some of its values replaced.
type tracer struct {
	ctx      context.Context
	release  string
	chartRef string
	version  string
	registry *registry.Client
	env      func(string) (string, bool)
	// user are the Application's user values; chart its chart.
	user  map[string]interface{}
	chart *chart.Chart
	// dir is the chart's directory, for a Git chart.
	dir string
}

func newTracer(ctx context.Context, release, chartRef, version string, reg *registry.Client, env func(string) (string, bool), user map[string]interface{}) (*tracer, error) {
	c, _, err := action.LoadChart(ctx, reg, chartRef, version)
	if err != nil {
		return nil, err
	}
	t := &tracer{ctx: ctx, release: release, chartRef: chartRef, version: version, registry: reg, env: env, user: user, chart: c}
	if !registry.IsOCI(chartRef) {
		t.dir = chartRef
	}
	return t, nil
}

// images renders with user values (and, if appVersion isn't "", the chart's
// appVersion replaced) and returns each service's image.
func (t *tracer) images(user map[string]interface{}, appVersion string) (map[string]string, error) {
	ref := t.chartRef
	if appVersion != "" {
		dir, err := t.withAppVersion(appVersion)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		ref = dir
	}
	manifest, _, err := renderManifest(action.RenderOptions{
		ChartPath: ref, Version: t.version, Registry: t.registry, Context: t.ctx,
		ReleaseName: t.release, Revision: 1, UserValues: user, Env: t.env,
	})
	if err != nil {
		return nil, err
	}
	svcs, err := images.Services(manifest, true, images.Policy{}, "")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range svcs {
		out[s.Name] = s.Image
	}
	return out, nil
}

// withAppVersion copies a Git chart to a temporary directory with its
// appVersion replaced.
func (t *tracer) withAppVersion(v string) (string, error) {
	if t.dir == "" {
		return "", fmt.Errorf("an OCI chart's appVersion can't be changed")
	}
	tmp, err := os.MkdirTemp("", "composecd-trace-")
	if err != nil {
		return "", err
	}
	if err := t.copyWithAppVersion(tmp, v); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// copyWithAppVersion copies the chart into dir and sets appVersion v in the
// copy's Chart.yaml, all through root-scoped file access.
func (t *tracer) copyWithAppVersion(dir, v string) error {
	if err := os.CopyFS(dir, os.DirFS(t.dir)); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := root.ReadFile("Chart.yaml")
	if err != nil {
		return err
	}
	doc, err := yamledit.Root(data)
	if err != nil {
		return err
	}
	if data, err = yamledit.Set(data, doc, []string{"appVersion"}, v); err != nil {
		return err
	}
	return root.WriteFile("Chart.yaml", data, 0o644)
}

// merged are the values the chart renders with.
func (t *tracer) merged() map[string]interface{} { return values.Coalesce(t.user, t.chart.Values) }

// sentinel is the placeholder for the i'th leaf tried: lower case letters and
// digits, so it stays a valid tag through any template.
func sentinel(i int) string { return "cdtrace" + strconv.Itoa(i) + "x" }

// trace finds the keys that set svc's tag in base, the images as the chart
// renders them now. With key set (a tag-key label) only that key is tried.
// More than one source means tracing is ambiguous.
func (t *tracer) trace(svc string, base map[string]string, key string) ([]tagSource, error) {
	img := base[svc]
	if img == "" {
		return nil, fmt.Errorf("the render has no image for %s", svc)
	}
	type leaf struct{ key, value string }
	var leaves []leaf
	merged := t.merged()
	if key != "" {
		var v string
		if key == KeyAppVersion {
			v = t.chart.Metadata.AppVersion
		} else {
			v, _ = leafString(lookupKey(merged, yamledit.Key(key)))
		}
		leaves = append(leaves, leaf{key, v})
	} else {
		for _, l := range scalarLeaves(merged, nil) {
			if strings.Contains(img, l[1]) {
				leaves = append(leaves, leaf{l[0], l[1]})
			}
		}
		if av := t.chart.Metadata.AppVersion; av != "" && strings.Contains(img, av) && t.dir != "" {
			leaves = append(leaves, leaf{KeyAppVersion, av})
		}
		if len(leaves) > maxTraceLeaves {
			return nil, fmt.Errorf("%d values appear in the image %s; set the %s label", len(leaves), img, tagKeyLabel)
		}
	}
	var found []tagSource
	for i, l := range leaves {
		s := sentinel(i)
		var got map[string]string
		var err error
		if l.key == KeyAppVersion {
			got, err = t.images(t.user, s)
		} else {
			got, err = t.images(values.Merge(t.user, nested(yamledit.Key(l.key), s)), "")
		}
		if err != nil {
			// A value the chart can't render without (a schema, a
			// required) isn't the tag's.
			continue
		}
		before, after, ok := strings.Cut(got[svc], s)
		if !ok || strings.Contains(after, s) || before+l.value+after != img {
			continue
		}
		src := tagSource{Key: l.key, prefix: before, suffix: after}
		for name, im := range got {
			if strings.Contains(im, s) {
				src.feeds = append(src.feeds, name)
			}
		}
		sort.Strings(src.feeds)
		found = append(found, src)
	}
	return found, nil
}

// tagKeyLabel is the service label naming the tag's values key.
const tagKeyLabel = config.LabelImageUpdates + ".tag-key"

// scalarLeaves lists v's scalar leaves as {dotted key, value} pairs, sorted.
func scalarLeaves(v map[string]interface{}, prefix []string) [][2]string {
	var out [][2]string
	for k, x := range v {
		path := append(append([]string{}, prefix...), k)
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, scalarLeaves(m, path)...)
			continue
		}
		if s, ok := leafString(x); ok && s != "" && !strings.Contains(k, ".") {
			out = append(out, [2]string{strings.Join(path, "."), s})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// leafString is a scalar value as a template prints it.
func leafString(x interface{}) (string, bool) {
	switch v := x.(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	}
	return "", false
}

// lookupKey is the value at path in v, or nil.
func lookupKey(v map[string]interface{}, path []string) interface{} {
	var cur interface{} = v
	for _, seg := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

// nested is a values map holding value at path.
func nested(path []string, value interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	cur := out
	for i, seg := range path {
		if i == len(path)-1 {
			cur[seg] = value
			break
		}
		next := map[string]interface{}{}
		cur[seg] = next
		cur = next
	}
	return out
}
