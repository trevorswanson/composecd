package web

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/features"
)

// Settings is an object's settings as a page shows them.
type Settings struct {
	Groups []features.Group
	// Origin is where the object is defined (file:line).
	Origin string
	// Owner is the ApplicationSet that generated an Application.
	Owner string
	// Error is why they can't be shown.
	Error string
	// Prefix is the path of the settings in their object, for a template's.
	Prefix string
}

// showFilters are the values of the show query parameter: every setting, or
// only those set (or inherited), or only those not.
var showFilters = []string{"", "set", "unset"}

// show keeps the settings the show filter asks for, dropping empty groups.
func show(groups []features.Group, filter string, err error) Settings {
	if err != nil {
		return Settings{Error: err.Error()}
	}
	if filter == "" {
		return Settings{Groups: groups}
	}
	var out []features.Group
	for _, g := range groups {
		kept := g
		kept.Settings = nil
		for _, s := range g.Settings {
			if (s.State != features.Default) == (filter == "set") {
				kept.Settings = append(kept.Settings, s)
			}
		}
		if kept.Settings != nil {
			out = append(out, kept)
		}
	}
	return Settings{Groups: out}
}

// appSettings are a's settings in Git, with its services' labels, and the
// ApplicationSet that generated it, if one did.
func (s *Server) appSettings(a controller.AppStatus, admin bool) ([]features.Group, string, error) {
	objs := s.C.Objects()
	var app *config.Application
	if objs != nil {
		app = objs.Application(a.Name)
	}
	if app == nil {
		return nil, "", fmt.Errorf("%s is not in Git", a.Name)
	}
	groups, err := features.Explain(features.Application, app, features.Context{
		Boot:     s.C.Bootstrap(),
		Mask:     func(text string) string { return s.C.Mask(a.Name, text) },
		KeysOnly: !admin,
	})
	if err != nil {
		return nil, "", err
	}
	return append(groups, features.ExplainLabels(serviceLabels(a.Objects))), app.Metadata.Labels[config.LabelApplicationSet], nil
}

// serviceLabels are the compose labels of each service as Git renders it.
func serviceLabels(objs []controller.Object) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, o := range objs {
		if o.Kind != controller.KindService || o.Desired == "" {
			continue
		}
		var svc struct {
			Labels yaml.Node `yaml:"labels"`
		}
		if yaml.Unmarshal([]byte(o.Desired), &svc) != nil {
			continue
		}
		labels := map[string]string{}
		switch svc.Labels.Kind {
		case yaml.MappingNode:
			_ = svc.Labels.Decode(&labels)
		case yaml.SequenceNode:
			var list []string
			_ = svc.Labels.Decode(&list)
			for _, l := range list {
				k, v, _ := strings.Cut(l, "=")
				labels[k] = v
			}
		}
		out[o.Name] = labels
	}
	return out
}

// hostSettings are the settings in Git of the host named name, and where it
// is defined.
func (s *Server) hostSettings(name string, admin bool) ([]features.Group, string, error) {
	objs := s.C.Objects()
	var h *config.Host
	if objs != nil {
		h = objs.Host(name)
	}
	if h == nil {
		return nil, "", fmt.Errorf("%s is not in Git", name)
	}
	groups, err := features.Explain(features.Host, h, features.Context{Boot: s.C.Bootstrap(), KeysOnly: !admin})
	return groups, h.Origin, err
}

// generated names the Applications in Git that the ApplicationSet named set
// generated.
func (s *Server) generated(set string) map[string]bool {
	out := map[string]bool{}
	if objs := s.C.Objects(); objs != nil {
		for _, a := range objs.Applications {
			if a.Metadata.Labels[config.LabelApplicationSet] == set {
				out[a.Metadata.Name] = true
			}
		}
	}
	return out
}

// appSetSettings are the settings in Git of the ApplicationSet named name,
// and of its template.
func (s *Server) appSetSettings(name string, admin bool) (set, template []features.Group, err error) {
	objs := s.C.Objects()
	var as *config.ApplicationSet
	if objs != nil {
		for _, candidate := range objs.ApplicationSets {
			if candidate.Metadata.Name == name {
				as = candidate
			}
		}
	}
	if as == nil {
		return nil, nil, fmt.Errorf("%s is not in Git", name)
	}
	ctx := features.Context{Boot: s.C.Bootstrap(), KeysOnly: !admin}
	if set, err = features.Explain(features.ApplicationSet, as, ctx); err != nil {
		return nil, nil, err
	}
	ctx.Prefix = "spec.template"
	template, err = features.Explain(features.Application, &as.Spec.Template, ctx)
	return set, template, err
}

// serverSettings are composecd.yaml's settings; what the file doesn't set
// shows as a default, though the server has filled it in.
func (s *Server) serverSettings() ([]features.Group, error) {
	b := s.C.Bootstrap()
	if b == nil {
		return nil, fmt.Errorf("the server's configuration is not available")
	}
	ctx := features.Context{}
	if b.Path != "" {
		// The file may have changed since the server read it; it only
		// tells what is set.
		if data, err := os.ReadFile(b.Path); err == nil {
			ctx.Written = data
		}
	}
	return features.Explain(features.Server, b, ctx)
}
