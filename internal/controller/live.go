package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composetpl/pkg/diff"
	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/source"
)

// Live is an object's state on its host next to its desired state.
type Live struct {
	Object Object `json:"object"`
	// Inspect is `docker inspect` of each engine object behind it: a service's
	// containers, or a network, volume or container. Configs and secrets have
	// none.
	Inspect []Inspected `json:"inspect,omitempty"`
	// Projected is the live state in compose's terms, the keys of Desired by
	// default and every key composecd can read back with all.
	Projected string `json:"projected,omitempty"`
	// Desired is the object's desired YAML in the same normalized form.
	Desired string `json:"desired,omitempty"`
	// Diff is a unified diff from Projected to Desired.
	Diff string `json:"diff,omitempty"`
	// Note says why there is no live state to compare.
	Note string `json:"note,omitempty"`
}

// Inspected is one engine object's inspect output as YAML, with its noisy
// parts folded away.
type Inspected struct {
	Name   string `json:"name"`
	YAML   string `json:"yaml"`
	Folded []Fold `json:"folded,omitempty"`
}

// Fold is a noisy part of inspect output, shown collapsed.
type Fold struct {
	Path   string `json:"path"`
	Fields int    `json:"fields"`
	YAML   string `json:"yaml"`
}

// liveTTL is how long inspect output is reused: opening a few dialogs or the
// object diff shouldn't run docker inspect over SSH each time.
const liveTTL = 30 * time.Second

type liveCache struct {
	mu      sync.Mutex
	entries map[string]liveEntry
}

type liveEntry struct {
	at  time.Time
	out map[string]interface{}
	err error
}

func (l *liveCache) inspect(ctx context.Context, key string, f func(context.Context) (map[string]interface{}, error)) (map[string]interface{}, error) {
	l.mu.Lock()
	if e, ok := l.entries[key]; ok && time.Since(e.at) < liveTTL {
		l.mu.Unlock()
		return e.out, e.err
	}
	l.mu.Unlock()
	out, err := f(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]liveEntry{}
	}
	for k, e := range l.entries {
		if time.Since(e.at) >= liveTTL {
			delete(l.entries, k)
		}
	}
	l.entries[key] = liveEntry{at: time.Now(), out: out, err: err}
	return out, err
}

// LiveObject reads object id of app from its host and compares it with Git.
// all includes live keys Git doesn't set; redact hides values of
// environment variables named like secrets, which composecd may never have
// seen and so can't mask.
func (c *Controller) LiveObject(ctx context.Context, name, id string, all, redact bool) (*Live, error) {
	app, err := c.application(name)
	if err != nil {
		return nil, err
	}
	st, err := c.App(name)
	if err != nil {
		return nil, err
	}
	var obj *Object
	for i := range st.Objects {
		if st.Objects[i].ID == id {
			obj = &st.Objects[i]
		}
	}
	if obj == nil {
		return nil, fmt.Errorf("object %q of %q: %w", id, name, ErrNotFound)
	}
	h := c.Hosts.Get(app.Spec.Destination.Host)
	if h == nil {
		return nil, fmt.Errorf("host %q is not defined", app.Spec.Destination.Host)
	}
	defer h.Use()()
	cfg := h.Configuration(source.Env(h.Spec, app), io.Discard)
	look := func(kind, object string) (map[string]interface{}, error) {
		return c.live.inspect(ctx, h.Name()+"\x00"+kind+"\x00"+object, func(ctx context.Context) (map[string]interface{}, error) {
			return cfg.Compose.Inspect(ctx, kind, object)
		})
	}

	out := &Live{Object: *obj}
	var engine []string
	kind := obj.Kind
	switch obj.Kind {
	case KindService:
		kind = KindContainer
		for _, o := range st.Objects {
			if o.Parent == obj.ID {
				engine = append(engine, o.LiveName)
			}
		}
	case KindContainer, KindNetwork, KindVolume:
		engine = []string{obj.LiveName}
	default:
		out.Note = "Compose has no engine object for a " + obj.Kind + "; it is mounted into the containers that use it."
	}
	var projected map[string]interface{}
	for _, e := range engine {
		got, err := look(kind, e)
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			continue
		}
		if redact {
			// A copy: the cache is shared with admins.
			got = deepCopy(got).(map[string]interface{})
			redactEnv(got)
		}
		ins, err := foldInspect(e, kind, got)
		if err != nil {
			return nil, err
		}
		ins.YAML, ins.Folded = c.Mask(name, ins.YAML), maskFolds(c, name, ins.Folded)
		out.Inspect = append(out.Inspect, ins)
		if projected != nil {
			continue
		}
		switch kind {
		case KindContainer:
			var image map[string]interface{}
			if ref, _ := got["Image"].(string); ref != "" {
				if image, err = look("image", ref); err != nil {
					return nil, err
				}
				if redact {
					// Tagged like the container's, so its defaults still match.
					image = deepCopy(image).(map[string]interface{})
					redactEnv(image)
				}
			}
			projected = projectContainer(got, image, name)
		case KindNetwork:
			projected = projectNetwork(got)
		case KindVolume:
			projected = projectVolume(got)
		}
	}
	if len(engine) > 0 && len(out.Inspect) == 0 {
		out.Note = "Not on the host."
	}

	desiredYAML := obj.Desired
	if obj.Kind == KindContainer {
		for _, o := range st.Objects {
			if o.ID == obj.Parent {
				desiredYAML = o.Desired
			}
		}
	}
	desired := normalizeDesired(obj.Kind, parseYAML(desiredYAML), h.ProjectDir(name))
	if redact && desired != nil {
		// The live side was tagged from the inspect output already.
		redactEnvMap(desired["environment"])
	}
	if projected != nil && !all && desired != nil {
		for k := range projected {
			if _, ok := desired[k]; !ok {
				delete(projected, k)
			}
		}
	}
	if desired != nil && projected != nil {
		// Keys composecd can't read back from the engine aren't compared.
		for k := range desired {
			if !projectable[obj.Kind][k] {
				delete(desired, k)
			}
		}
	}
	if out.Projected, err = encodeYAML(projected); err != nil {
		return nil, err
	}
	if out.Desired, err = encodeYAML(desired); err != nil {
		return nil, err
	}
	out.Projected, out.Desired = c.Mask(name, out.Projected), c.Mask(name, out.Desired)
	if out.Projected != out.Desired {
		var buf bytes.Buffer
		ch := diff.Change{Resource: id, Op: diff.Changed, Old: out.Projected, New: out.Desired}
		if err := diff.Print(&buf, []diff.Change{ch}, diff.PrintOptions{Release: name, Context: 3}); err != nil {
			return nil, err
		}
		text := buf.String()
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		out.Diff = strings.TrimRight(text, "\n") + "\n"
	}
	return out, nil
}

func maskFolds(c *Controller, name string, folds []Fold) []Fold {
	for i := range folds {
		folds[i].YAML = c.Mask(name, folds[i].YAML)
	}
	return folds
}

func encodeYAML(v map[string]interface{}) (string, error) {
	if v == nil {
		return "", nil
	}
	if len(v) == 0 {
		return "{}\n", nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), enc.Close()
}

// noisy are the parts of inspect output folded away by default, like
// Kubernetes' managedFields in Argo CD: engine bookkeeping nobody configures.
var noisy = map[string][]string{
	KindContainer: {"Id", "Created", "Path", "Args", "ResolvConfPath", "HostnamePath", "HostsPath", "LogPath", "GraphDriver",
		"Driver", "Platform", "MountLabel", "ProcessLabel", "AppArmorProfile", "ExecIDs", "SizeRw", "SizeRootFs",
		"ImageManifestDescriptor", "State.Health.Log", "NetworkSettings.SandboxID", "NetworkSettings.SandboxKey",
		"NetworkSettings.Ports", "NetworkSettings.Bridge", "NetworkSettings.HairpinMode", "NetworkSettings.LinkLocalIPv6Address",
		"NetworkSettings.LinkLocalIPv6PrefixLen", "NetworkSettings.SecondaryIPAddresses", "NetworkSettings.SecondaryIPv6Addresses"},
	KindNetwork: {"Id", "Created", "Scope", "ConfigFrom", "ConfigOnly", "Containers", "Options", "Peers", "Services"},
	KindVolume:  {"CreatedAt", "Mountpoint", "Scope", "Status", "UsageData"},
}

// foldInspect renders inspect output as YAML with the noisy parts, compose's
// own labels and HostConfig's unset fields moved into folds.
func foldInspect(name, kind string, in map[string]interface{}) (Inspected, error) {
	m := deepCopy(in).(map[string]interface{})
	var folds []Fold
	// The health check log is the first thing to read when a check hasn't passed.
	skip := ""
	if h, _ := lookup(m, []string{"State", "Health", "Status"}); kind == KindContainer && h != nil && h != "healthy" {
		skip = "State.Health.Log"
	}
	add := func(p string, v interface{}) error {
		y, err := encodeYAML(map[string]interface{}{path.Base(strings.ReplaceAll(p, ".", "/")): v})
		if err != nil {
			return err
		}
		folds = append(folds, Fold{Path: p, Fields: fieldCount(v), YAML: y})
		return nil
	}
	// Top-level bookkeeping folds together; nested noise folds one by one.
	meta := map[string]interface{}{}
	for _, p := range noisy[kind] {
		if p == skip {
			continue
		}
		v, ok := cut(m, strings.Split(p, "."))
		switch {
		case !ok:
		case !strings.Contains(p, "."):
			meta[p] = v
		default:
			if err := add(p, v); err != nil {
				return Inspected{}, err
			}
		}
	}
	if len(meta) > 0 {
		y, err := encodeYAML(meta)
		if err != nil {
			return Inspected{}, err
		}
		folds = append([]Fold{{Path: "Engine metadata", Fields: len(meta), YAML: y}}, folds...)
	}
	for _, labels := range []string{"Config.Labels", "Labels"} {
		parent, ok := lookup(m, strings.Split(labels, "."))
		l, isMap := parent.(map[string]interface{})
		if !ok || !isMap {
			continue
		}
		compose := map[string]interface{}{}
		for k, v := range l {
			if strings.HasPrefix(k, "com.docker.compose.") {
				compose[k] = v
				delete(l, k)
			}
		}
		if len(compose) > 0 {
			if err := add(labels+" (com.docker.compose.*)", compose); err != nil {
				return Inspected{}, err
			}
		}
	}
	if hc, ok := m["HostConfig"].(map[string]interface{}); ok {
		unset := map[string]interface{}{}
		for k, v := range hc {
			if isZero(v) {
				unset[k] = v
				delete(hc, k)
			}
		}
		if len(unset) > 0 {
			if err := add("HostConfig (unset)", unset); err != nil {
				return Inspected{}, err
			}
		}
	}
	y, err := encodeYAML(m)
	return Inspected{Name: name, YAML: y, Folded: folds}, err
}

func fieldCount(v interface{}) int {
	switch x := v.(type) {
	case map[string]interface{}:
		return len(x)
	case []interface{}:
		return len(x)
	}
	return 1
}

func isZero(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case []interface{}:
		return len(x) == 0
	case map[string]interface{}:
		for _, e := range x {
			if !isZero(e) {
				return false
			}
		}
		return true
	}
	return false
}

func lookup(m map[string]interface{}, p []string) (interface{}, bool) {
	var cur interface{} = m
	for _, k := range p {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// cut removes p from m and returns what was there.
func cut(m map[string]interface{}, p []string) (interface{}, bool) {
	parent, ok := lookup(m, p[:len(p)-1])
	mm, isMap := parent.(map[string]interface{})
	if !ok || !isMap {
		return nil, false
	}
	v, ok := mm[p[len(p)-1]]
	delete(mm, p[len(p)-1])
	return v, ok
}

func deepCopy(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

// redactKey keys the tags that stand in for hidden values: equal values get
// equal tags, so a diff still shows a change, but a tag can't be reversed by
// guessing values, and it changes when composecd restarts.
var redactKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

// redactValue is the tag shown instead of v.
func redactValue(v string) string {
	m := hmac.New(sha256.New, redactKey)
	m.Write([]byte(v))
	return "<redacted " + hex.EncodeToString(m.Sum(nil))[:8] + ">"
}

// redactEnv hides every environment value in a container's inspect output: a
// live container can hold values composecd never decrypted, so it can't tell
// which are secret.
func redactEnv(in map[string]interface{}) {
	cfg, _ := in["Config"].(map[string]interface{})
	env, _ := cfg["Env"].([]interface{})
	for i, e := range env {
		if s, ok := e.(string); ok {
			if k, v, ok := strings.Cut(s, "="); ok {
				env[i] = k + "=" + redactValue(v)
			}
		}
	}
}

func redactEnvMap(v interface{}) {
	if env, ok := v.(map[string]interface{}); ok {
		for k, e := range env {
			env[k] = redactValue(fmt.Sprint(e))
		}
	}
}

// projectable are the compose keys each kind's projection reads back.
var projectable = map[string]map[string]bool{
	KindService: set("image", "command", "entrypoint", "environment", "ports", "volumes", "networks", "labels", "restart",
		"healthcheck", "user", "working_dir", "hostname", "cap_add", "cap_drop", "extra_hosts", "privileged", "read_only",
		"init", "tty", "stdin_open", "stop_signal", "dns", "shm_size", "mem_limit", "cpus", "container_name", "network_mode"),
	KindNetwork: set("driver", "internal", "attachable", "enable_ipv6", "labels", "driver_opts"),
	KindVolume:  set("driver", "driver_opts", "labels"),
}

func set(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func init() { projectable[KindContainer] = projectable[KindService] }

// projectContainer reads a container's inspect output back into compose
// service keys, leaving out what its image sets by default.
func projectContainer(in, image map[string]interface{}, project string) map[string]interface{} {
	cfg, _ := in["Config"].(map[string]interface{})
	hc, _ := in["HostConfig"].(map[string]interface{})
	imgCfg, _ := image["Config"].(map[string]interface{})
	out := map[string]interface{}{}
	put := func(k string, v interface{}) {
		if !isZero(v) {
			out[k] = v
		}
	}
	put("image", cfg["Image"])
	if !sameValue(cfg["Cmd"], imgCfg["Cmd"]) {
		put("command", cfg["Cmd"])
	}
	if !sameValue(cfg["Entrypoint"], imgCfg["Entrypoint"]) {
		put("entrypoint", cfg["Entrypoint"])
	}
	defaults := map[string]bool{}
	for _, e := range asList(imgCfg["Env"]) {
		defaults[fmt.Sprint(e)] = true
	}
	env := map[string]interface{}{}
	for _, e := range asList(cfg["Env"]) {
		s := fmt.Sprint(e)
		if defaults[s] {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		env[k] = v
	}
	put("environment", env)
	labels := map[string]interface{}{}
	imgLabels, _ := imgCfg["Labels"].(map[string]interface{})
	if l, ok := cfg["Labels"].(map[string]interface{}); ok {
		for k, v := range l {
			if strings.HasPrefix(k, "com.docker.compose.") || imgLabels[k] == v {
				continue
			}
			labels[k] = v
		}
	}
	put("labels", labels)
	if wd, _ := cfg["WorkingDir"].(string); wd != "" && wd != imgCfg["WorkingDir"] {
		out["working_dir"] = wd
	}
	if u, _ := cfg["User"].(string); u != "" && u != imgCfg["User"] {
		out["user"] = u
	}
	put("hostname", cfg["Hostname"])
	put("tty", cfg["Tty"])
	put("stdin_open", cfg["OpenStdin"])
	if s, _ := cfg["StopSignal"].(string); s != "" && s != imgCfg["StopSignal"] {
		out["stop_signal"] = s
	}
	if hcheck, ok := cfg["Healthcheck"].(map[string]interface{}); ok && !sameValue(hcheck, imgCfg["Healthcheck"]) {
		h := map[string]interface{}{}
		if t := asList(hcheck["Test"]); len(t) > 0 {
			h["test"] = t
		}
		for src, dst := range map[string]string{"Interval": "interval", "Timeout": "timeout", "StartPeriod": "start_period", "StartInterval": "start_interval"} {
			if n, ok := hcheck[src].(float64); ok && n > 0 {
				h[dst] = time.Duration(n).String()
			}
		}
		if n, ok := hcheck["Retries"].(float64); ok && n > 0 {
			h["retries"] = strconv.Itoa(int(n))
		}
		put("healthcheck", h)
	}
	if rp, ok := hc["RestartPolicy"].(map[string]interface{}); ok {
		if n, _ := rp["Name"].(string); n != "" && n != "no" {
			out["restart"] = n
		}
	}
	for src, dst := range map[string]string{"CapAdd": "cap_add", "CapDrop": "cap_drop", "ExtraHosts": "extra_hosts", "Dns": "dns"} {
		if l := asList(hc[src]); len(l) > 0 {
			out[dst] = sortedStrings(l)
		}
	}
	put("privileged", hc["Privileged"])
	put("read_only", hc["ReadonlyRootfs"])
	put("init", hc["Init"])
	if mode, _ := hc["NetworkMode"].(string); mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") || strings.HasPrefix(mode, "service:") {
		out["network_mode"] = mode
	}
	if n, ok := hc["Memory"].(float64); ok && n > 0 {
		out["mem_limit"] = strconv.FormatInt(int64(n), 10)
	}
	if n, ok := hc["NanoCpus"].(float64); ok && n > 0 {
		out["cpus"] = strconv.FormatFloat(n/1e9, 'f', -1, 64)
	}
	var ports []interface{}
	if pb, ok := hc["PortBindings"].(map[string]interface{}); ok {
		for spec, binds := range pb {
			target, proto, _ := strings.Cut(spec, "/")
			for _, b := range asList(binds) {
				bm, _ := b.(map[string]interface{})
				p := map[string]interface{}{"target": target, "protocol": proto}
				if hp, _ := bm["HostPort"].(string); hp != "" {
					p["published"] = hp
				}
				if ip, _ := bm["HostIp"].(string); ip != "" && ip != "0.0.0.0" && ip != "::" {
					p["host_ip"] = ip
				}
				ports = append(ports, p)
			}
		}
	}
	put("ports", sortMaps(ports))
	var mounts []interface{}
	for _, m := range asList(in["Mounts"]) {
		mm, _ := m.(map[string]interface{})
		t, _ := mm["Type"].(string)
		v := map[string]interface{}{"type": t, "target": mm["Destination"]}
		switch t {
		case "volume":
			src, _ := mm["Name"].(string)
			v["source"] = strings.TrimPrefix(src, project+"_")
			if len(src) == 64 && strings.Trim(src, "0123456789abcdef") == "" {
				continue // an anonymous volume the image declares
			}
		case "bind":
			v["source"] = mm["Source"]
		default:
			continue
		}
		if rw, ok := mm["RW"].(bool); ok && !rw {
			v["read_only"] = true
		}
		mounts = append(mounts, v)
	}
	put("volumes", sortMaps(mounts))
	var nets []interface{}
	if ns, ok := in["NetworkSettings"].(map[string]interface{}); ok {
		if n, ok := ns["Networks"].(map[string]interface{}); ok {
			for k := range n {
				nets = append(nets, strings.TrimPrefix(k, project+"_"))
			}
		}
	}
	if _, own := out["network_mode"]; !own {
		put("networks", sortedStrings(nets))
	}
	if name, _ := in["Name"].(string); name != "" {
		out["container_name"] = strings.TrimPrefix(name, "/")
	}
	return out
}

func projectNetwork(in map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	if d, _ := in["Driver"].(string); d != "" {
		out["driver"] = d
	}
	for src, dst := range map[string]string{"Internal": "internal", "Attachable": "attachable", "EnableIPv6": "enable_ipv6"} {
		if b, _ := in[src].(bool); b {
			out[dst] = true
		}
	}
	if o, ok := in["Options"].(map[string]interface{}); ok && len(o) > 0 {
		out["driver_opts"] = o
	}
	if l := userLabels(in["Labels"]); len(l) > 0 {
		out["labels"] = l
	}
	return out
}

func projectVolume(in map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	if d, _ := in["Driver"].(string); d != "" {
		out["driver"] = d
	}
	if o, ok := in["Options"].(map[string]interface{}); ok && len(o) > 0 {
		out["driver_opts"] = o
	}
	if l := userLabels(in["Labels"]); len(l) > 0 {
		out["labels"] = l
	}
	return out
}

func userLabels(v interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	if l, ok := v.(map[string]interface{}); ok {
		for k, e := range l {
			if !strings.HasPrefix(k, "com.docker.compose.") {
				out[k] = e
			}
		}
	}
	return out
}

// normalizeDesired brings a desired compose object into the projection's
// form: lists of KEY=VALUE become maps, short port and volume syntax becomes
// long, strings that compose splits become lists, and scalars become strings.
func normalizeDesired(kind string, spec map[string]interface{}, projectDir string) map[string]interface{} {
	if spec == nil {
		return nil
	}
	out := map[string]interface{}{}
	for k, v := range spec {
		out[k] = v
	}
	if kind != KindService && kind != KindContainer {
		if l := toMap(out["labels"]); l != nil {
			out["labels"] = l
		}
		if o := toMap(out["driver_opts"]); o != nil {
			out["driver_opts"] = o
		}
		return out
	}
	for _, k := range []string{"environment", "labels"} {
		if m := toMap(out[k]); m != nil {
			out[k] = m
		}
	}
	for _, k := range []string{"command", "entrypoint"} {
		if s, ok := out[k].(string); ok {
			out[k] = toList(splitShell(s))
		} else if l := asList(out[k]); l != nil {
			out[k] = stringList(l)
		}
	}
	if r, ok := out["restart"].(string); ok && r == "no" {
		delete(out, "restart")
	}
	if n, ok := out["networks"]; ok {
		out["networks"] = toList(keysOrItems(n, ""))
	}
	for _, k := range []string{"cap_add", "cap_drop", "extra_hosts", "dns"} {
		if l := asList(out[k]); l != nil {
			out[k] = sortedStrings(l)
		} else if m, ok := out[k].(map[string]interface{}); ok && k == "extra_hosts" {
			var hosts []interface{}
			for h, ip := range m {
				hosts = append(hosts, fmt.Sprintf("%s:%v", h, ip))
			}
			out[k] = sortedStrings(hosts)
		}
	}
	for _, k := range []string{"privileged", "read_only", "init", "tty", "stdin_open"} {
		if b, ok := out[k].(bool); ok && !b {
			delete(out, k)
		}
	}
	if c, ok := out["cpus"]; ok {
		out["cpus"] = fmt.Sprint(c)
	}
	if hc, ok := out["healthcheck"].(map[string]interface{}); ok {
		h := map[string]interface{}{}
		for k, v := range hc {
			switch k {
			case "test":
				if s, ok := v.(string); ok {
					h[k] = []interface{}{"CMD-SHELL", s}
				} else {
					h[k] = stringList(asList(v))
				}
			case "interval", "timeout", "start_period", "start_interval":
				if d, err := time.ParseDuration(fmt.Sprint(v)); err == nil {
					h[k] = d.String()
				} else {
					h[k] = fmt.Sprint(v)
				}
			default:
				h[k] = fmt.Sprint(v)
			}
		}
		out["healthcheck"] = h
	}
	if ports := asList(out["ports"]); ports != nil {
		var long []interface{}
		for _, p := range ports {
			long = append(long, longPort(p)...)
		}
		out["ports"] = sortMaps(long)
	}
	if vols := asList(out["volumes"]); vols != nil {
		var long []interface{}
		for _, v := range vols {
			if lv := longVolume(v, projectDir); lv != nil {
				long = append(long, lv)
			}
		}
		out["volumes"] = sortMaps(long)
	}
	return out
}

// longPort is compose's long syntax for a short port ("127.0.0.1:8080:80/udp").
func longPort(p interface{}) []interface{} {
	if m, ok := p.(map[string]interface{}); ok {
		out := map[string]interface{}{"target": fmt.Sprint(m["target"]), "protocol": "tcp"}
		if v, ok := m["published"]; ok {
			out["published"] = fmt.Sprint(v)
		}
		if v, ok := m["protocol"]; ok {
			out["protocol"] = fmt.Sprint(v)
		}
		if v, ok := m["host_ip"]; ok {
			out["host_ip"] = fmt.Sprint(v)
		}
		return []interface{}{anyHost(out)}
	}
	s := fmt.Sprint(p)
	spec, proto, ok := strings.Cut(s, "/")
	if !ok {
		proto = "tcp"
	}
	parts := strings.Split(spec, ":")
	out := map[string]interface{}{"protocol": proto}
	switch len(parts) {
	case 1:
		out["target"] = parts[0]
	case 2:
		out["published"], out["target"] = parts[0], parts[1]
	default:
		out["host_ip"], out["published"], out["target"] = strings.Join(parts[:len(parts)-2], ":"), parts[len(parts)-2], parts[len(parts)-1]
	}
	if hp, _ := out["published"].(string); hp == "" {
		delete(out, "published")
	}
	return []interface{}{anyHost(out)}
}

// anyHost drops a host_ip that binds every address, as the live side does:
// "0.0.0.0:80:80" and "80:80" publish the same port.
func anyHost(p map[string]interface{}) map[string]interface{} {
	if ip, _ := p["host_ip"].(string); ip == "" || ip == "0.0.0.0" || ip == "::" {
		delete(p, "host_ip")
	}
	return p
}

// longVolume is compose's long syntax for a short volume ("data:/data:ro"),
// with a relative bind source resolved against the project directory.
func longVolume(v interface{}, projectDir string) interface{} {
	out := map[string]interface{}{}
	switch x := v.(type) {
	case map[string]interface{}:
		t, _ := x["type"].(string)
		if t == "" {
			t = "volume"
		}
		if t != "volume" && t != "bind" {
			return nil
		}
		out["type"], out["target"] = t, fmt.Sprint(x["target"])
		if src, ok := x["source"].(string); ok {
			out["source"] = src
		}
		if ro, _ := x["read_only"].(bool); ro {
			out["read_only"] = true
		}
	case string:
		parts := strings.Split(x, ":")
		if len(parts) < 2 {
			return nil // anonymous volume
		}
		out["source"], out["target"] = parts[0], parts[1]
		if len(parts) > 2 && strings.Contains(parts[2], "ro") {
			out["read_only"] = true
		}
		out["type"] = "volume"
		if strings.ContainsAny(parts[0][:1], "/.~$") {
			out["type"] = "bind"
		}
	default:
		return nil
	}
	if src, _ := out["source"].(string); out["type"] == "bind" && (strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || src == ".") {
		out["source"] = path.Join(projectDir, src)
	}
	return out
}

func toMap(v interface{}) map[string]interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, e := range x {
			if e == nil {
				continue
			}
			out[k] = fmt.Sprint(e)
		}
		return out
	case []interface{}:
		out := map[string]interface{}{}
		for _, e := range x {
			k, val, ok := strings.Cut(fmt.Sprint(e), "=")
			if ok {
				out[k] = val
			}
		}
		return out
	}
	return nil
}

func asList(v interface{}) []interface{} {
	l, _ := v.([]interface{})
	return l
}

func toList(s []string) []interface{} {
	out := make([]interface{}, len(s))
	for i, e := range s {
		out[i] = e
	}
	return out
}

func stringList(l []interface{}) []interface{} {
	out := make([]interface{}, len(l))
	for i, e := range l {
		out[i] = fmt.Sprint(e)
	}
	return out
}

func sortedStrings(l []interface{}) []interface{} {
	out := stringList(l)
	sort.Slice(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
	return out
}

// sortMaps orders a list of maps by their YAML, so lists compare regardless
// of the order compose or the engine wrote them in.
func sortMaps(l []interface{}) []interface{} {
	keys := make([]string, len(l))
	for i, e := range l {
		b, _ := yaml.Marshal(e)
		keys[i] = string(b)
	}
	idx := make([]int, len(l))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })
	out := make([]interface{}, len(l))
	for i, j := range idx {
		out[i] = l[j]
	}
	return out
}

func sameValue(a, b interface{}) bool {
	if isZero(a) && isZero(b) {
		return true
	}
	ay, _ := yaml.Marshal(a)
	by, _ := yaml.Marshal(b)
	return bytes.Equal(ay, by)
}

// splitShell splits a command string the way compose does: on spaces,
// keeping quoted parts together.
func splitShell(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	started := false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}
