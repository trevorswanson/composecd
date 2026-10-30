package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/diff"
	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/health"
)

// ObjectSync is how one compose object compares with Git.
type ObjectSync string

const (
	ObjectSynced    ObjectSync = "Synced"
	ObjectOutOfSync ObjectSync = "OutOfSync"
	// ObjectMissing objects are in Git but not on the host.
	ObjectMissing ObjectSync = "Missing"
	// ObjectPrune objects are on the host (or in the release) but not in Git.
	ObjectPrune ObjectSync = "Prune"
)

// Object kinds.
const (
	KindService   = "service"
	KindNetwork   = "network"
	KindVolume    = "volume"
	KindConfig    = "config"
	KindSecret    = "secret"
	KindContainer = "container"
)

// sectionKinds maps compose's top-level sections to the kinds of their entries.
var sectionKinds = map[string]string{
	"services": KindService, "networks": KindNetwork, "volumes": KindVolume, "configs": KindConfig, "secrets": KindSecret,
}

// Object is one compose object of an app (a service, network, volume, config
// or secret) or one of its containers, and how it compares with Git.
type Object struct {
	// ID is "<section>/<name>" ("services/web"), as Change.Resource names it,
	// or "containers/<container name>".
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Parent is a container's service.
	Parent string     `json:"parent,omitempty"`
	Sync   ObjectSync `json:"sync"`
	// Health is a service's or container's health from its containers.
	Health  health.Status `json:"health,omitempty"`
	Message string        `json:"message,omitempty"`
	Image   string        `json:"image,omitempty"`
	// LiveName is the engine's name for a network, volume or container.
	LiveName string `json:"liveName,omitempty"`
	// External objects belong to no project; composecd only uses them.
	External bool `json:"external,omitempty"`
	// Uses are the networks, volumes, configs and secrets a service uses.
	Uses []string `json:"uses,omitempty"`
	// DependsOn are the services a service starts after.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Desired and Deployed are the object's YAML in Git and in the deployed
	// release ("" where it has none).
	Desired  string `json:"desired,omitempty"`
	Deployed string `json:"deployed,omitempty"`
}

// objectInput is what buildObjects compares.
type objectInput struct {
	project           string
	desired, deployed string
	changes           []Change
	containers        []compose.Container
	drift             []action.ServiceDrift
	// live is false when the containers couldn't be read: nothing is then
	// judged missing from the host.
	live bool
	// inspect looks a network or volume up on the host; nil skips the check.
	inspect func(kind, name string) (map[string]interface{}, error)
	// details is the last health check result of each container whose check
	// hasn't passed, by container name (see healthDetails).
	details map[string]string
}

// buildObjects lists an app's objects from its desired and deployed
// manifests, their changes and the live containers.
func buildObjects(in objectInput) ([]Object, error) {
	want, err := resourceYAML(in.desired, true)
	if err != nil {
		return nil, err
	}
	had, err := resourceYAML(in.deployed, false)
	if err != nil {
		return nil, err
	}
	ops := map[string]string{}
	for _, ch := range in.changes {
		ops[ch.Resource] = ch.Op
	}
	byService := map[string][]compose.Container{}
	for _, ct := range in.containers {
		byService[ct.Service] = append(byService[ct.Service], ct)
	}
	driftOf := map[string]string{}
	for _, d := range in.drift {
		if d.Reason == action.DriftMissing || d.Reason == action.DriftChanged {
			driftOf[d.Service] = d.Reason
		}
		for _, name := range d.Containers {
			driftOf["containers/"+name] = d.Reason
		}
	}

	ids := map[string]bool{}
	for id := range want {
		ids[id] = true
	}
	for id := range had {
		ids[id] = true
	}
	objs := map[string]*Object{}
	ownNetwork := map[string]bool{}
	for id := range ids {
		section, name, _ := strings.Cut(id, "/")
		o := &Object{ID: id, Kind: sectionKinds[section], Name: name, Desired: want[id], Deployed: had[id], Sync: ObjectSynced}
		spec := parseYAML(o.Desired)
		if spec == nil {
			spec = parseYAML(o.Deployed)
		}
		switch ops[id] {
		case string(diff.Removed):
			o.Sync = ObjectPrune
		case string(diff.Changed):
			o.Sync = ObjectOutOfSync
		case string(diff.Added), OpMissing:
			o.Sync = ObjectMissing
		}
		switch o.Kind {
		case KindService:
			o.Image, _ = spec["image"].(string)
			o.Uses, o.DependsOn = serviceRefs(spec)
			_, ownNetwork[id] = spec["network_mode"]
		case KindNetwork, KindVolume, KindConfig, KindSecret:
			o.External = truthy(spec["external"])
			o.LiveName = in.project + "_" + name
			if n, ok := spec["name"].(string); ok && n != "" {
				o.LiveName = n
			} else if o.External {
				o.LiveName = name
			}
		}
		objs[id] = o
	}
	// Services that don't name a network join the project's default one.
	for _, o := range objs {
		if o.Kind == KindService && !hasNetwork(o.Uses) && !ownNetwork[o.ID] && o.Desired != "" {
			o.Uses = append(o.Uses, "networks/default")
			if objs["networks/default"] == nil {
				objs["networks/default"] = &Object{ID: "networks/default", Kind: KindNetwork, Name: "default",
					LiveName: in.project + "_default", Sync: ObjectSynced, Message: "created for services that name no network"}
			}
		}
	}

	for _, o := range objs {
		switch o.Kind {
		case KindService:
			cts := byService[o.Name]
			o.Health, o.Message = serviceHealth(cts)
			o.Message = withDetails(o.Message, cts, in.details)
			switch {
			case o.Sync == ObjectPrune:
			case driftOf[o.Name] == action.DriftMissing, in.live && len(cts) == 0 && o.Desired != "":
				o.Sync = ObjectMissing
			case o.Sync == ObjectMissing && len(cts) > 0:
				// Containers started before the first sync: compose adopts them.
				o.Sync = ObjectOutOfSync
			case driftOf[o.Name] == action.DriftChanged:
				o.Sync = ObjectOutOfSync
			}
			if o.Sync == ObjectMissing {
				o.Health, o.Message = health.Missing, "no containers"
			}
		case KindNetwork, KindVolume:
			if o.External || in.inspect == nil || !in.live {
				continue
			}
			got, err := in.inspect(o.Kind, o.LiveName)
			switch {
			case err != nil:
				o.Message = err.Error()
			case len(got) == 0 && o.Sync != ObjectPrune:
				o.Sync = ObjectMissing
			case len(got) > 0 && o.Sync == ObjectMissing:
				o.Sync = ObjectSynced
			}
		}
	}

	for _, ct := range in.containers {
		svc := "services/" + ct.Service
		if objs[svc] == nil {
			// A service in neither manifest: an orphan, or a hook's container.
			sync := ObjectSynced
			if driftOf["containers/"+ct.Name] == action.DriftOrphaned {
				sync = ObjectPrune
			}
			objs[svc] = &Object{ID: svc, Kind: KindService, Name: ct.Service, Sync: sync, Image: ct.Image}
			objs[svc].Health, objs[svc].Message = serviceHealth(byService[ct.Service])
			objs[svc].Message = withDetails(objs[svc].Message, byService[ct.Service], in.details)
		}
		o := &Object{ID: "containers/" + ct.Name, Kind: KindContainer, Name: ct.Name, Parent: svc, LiveName: ct.Name,
			Image: ct.Image, Sync: ObjectSynced}
		o.Health, o.Message = health.Container(ct)
		if o.Message == "" {
			o.Message = ct.Status
		}
		o.Message = withDetails(o.Message, []compose.Container{ct}, in.details)
		switch driftOf[o.ID] {
		case action.DriftChanged:
			o.Sync = ObjectOutOfSync
		case action.DriftOrphaned:
			o.Sync = ObjectPrune
		}
		if objs[svc].Sync == ObjectPrune {
			o.Sync = ObjectPrune
		}
		objs[o.ID] = o
	}

	out := make([]Object, 0, len(objs))
	for _, o := range objs {
		var uses []string
		for _, u := range o.Uses {
			if objs[u] != nil {
				uses = append(uses, u)
			}
		}
		sort.Strings(uses)
		o.Uses = uses
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// resourceYAML splits a manifest into each section entry's YAML, by ID.
func resourceYAML(manifest string, isNew bool) (map[string]string, error) {
	var changes []diff.Change
	var err error
	if isNew {
		changes, err = diff.Manifests("", manifest)
	} else {
		changes, err = diff.Manifests(manifest, "")
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ch := range changes {
		section, _, ok := strings.Cut(ch.Resource, "/")
		if !ok || sectionKinds[section] == "" {
			continue
		}
		if isNew {
			out[ch.Resource] = ch.New
		} else {
			out[ch.Resource] = ch.Old
		}
	}
	return out, nil
}

func parseYAML(s string) map[string]interface{} {
	if s == "" {
		return nil
	}
	var m map[string]interface{}
	if yaml.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return m
}

func truthy(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case map[string]interface{}:
		return true // external: {name: ...}, the legacy form
	}
	return false
}

// serviceRefs reads the objects a service spec uses and the services it
// depends on, in compose's short and long forms.
func serviceRefs(spec map[string]interface{}) (uses, deps []string) {
	add := func(section, name string) {
		if name != "" {
			uses = append(uses, section+"/"+name)
		}
	}
	for _, n := range keysOrItems(spec["networks"], "") {
		add("networks", n)
	}
	if vols, ok := spec["volumes"].([]interface{}); ok {
		for _, v := range vols {
			switch x := v.(type) {
			case string:
				src, _, ok := strings.Cut(x, ":")
				if ok && src != "" && !strings.ContainsAny(src[:1], "/.~$") {
					add("volumes", src)
				}
			case map[string]interface{}:
				if t, _ := x["type"].(string); t == "volume" || t == "" {
					if src, _ := x["source"].(string); src != "" && !strings.ContainsAny(src[:1], "/.~$") {
						add("volumes", src)
					}
				}
			}
		}
	}
	for _, n := range keysOrItems(spec["configs"], "source") {
		add("configs", n)
	}
	for _, n := range keysOrItems(spec["secrets"], "source") {
		add("secrets", n)
	}
	for _, n := range keysOrItems(spec["depends_on"], "") {
		deps = append(deps, "services/"+n)
	}
	sort.Strings(uses)
	sort.Strings(deps)
	return uses, deps
}

// keysOrItems is a compose list's names: a map's keys, or a list's strings
// (and, with field, the field of its map items).
func keysOrItems(v interface{}, field string) []string {
	var out []string
	switch x := v.(type) {
	case map[string]interface{}:
		for k := range x {
			out = append(out, k)
		}
	case []interface{}:
		for _, item := range x {
			switch it := item.(type) {
			case string:
				out = append(out, it)
			case map[string]interface{}:
				if s, ok := it[field].(string); ok && field != "" {
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func hasNetwork(uses []string) bool {
	for _, u := range uses {
		if strings.HasPrefix(u, "networks/") {
			return true
		}
	}
	return false
}

// serviceHealth is the worst of a service's containers.
func serviceHealth(cts []compose.Container) (health.Status, string) {
	if len(cts) == 0 {
		return "", ""
	}
	return health.Assess(true, cts)
}

// failingCheck says whether a container's health check hasn't passed.
func failingCheck(c compose.Container) bool {
	return c.State == "running" && (c.Health == "starting" || c.Health == "unhealthy")
}

// healthDetails reads the last health check result of each container whose
// check hasn't passed, by container name. A container that can't be inspected
// is left out.
func healthDetails(inspect func(kind, name string) (map[string]interface{}, error), cts []compose.Container) map[string]string {
	out := map[string]string{}
	if inspect == nil {
		return out
	}
	for _, c := range cts {
		if !failingCheck(c) {
			continue
		}
		in, err := inspect(KindContainer, c.Name)
		if err != nil {
			continue
		}
		out[c.Name] = lastCheck(in)
	}
	return out
}

// lastCheck describes the newest entry of State.Health.Log in one line.
func lastCheck(inspected map[string]interface{}) string {
	v, _ := lookup(inspected, []string{"State", "Health", "Log"})
	log, _ := v.([]interface{})
	if len(log) == 0 {
		return "no health check result yet"
	}
	e, _ := log[len(log)-1].(map[string]interface{})
	code, _ := e["ExitCode"].(float64)
	out, _ := e["Output"].(string)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	out = strings.TrimSpace(lines[len(lines)-1])
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	msg := fmt.Sprintf("last health check exited %d", int(code))
	if out != "" {
		msg += ": " + out
	}
	return msg
}

// withDetails adds the health check results of cts to a health message.
func withDetails(msg string, cts []compose.Container, details map[string]string) string {
	var parts []string
	for _, c := range cts {
		if d := details[c.Name]; d != "" {
			parts = append(parts, c.Name+": "+d)
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return msg
	}
	return msg + " (" + strings.Join(parts, "; ") + ")"
}

// objectsFor builds st's objects, looking networks and volumes up on the host.
func objectsFor(ctx context.Context, cfg *action.Configuration, in objectInput) ([]Object, error) {
	in.inspect = func(kind, name string) (map[string]interface{}, error) {
		return cfg.Compose.Inspect(ctx, kind, name)
	}
	return buildObjects(in)
}
