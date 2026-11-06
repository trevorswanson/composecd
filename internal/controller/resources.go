package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/hosts"
)

// HostResources lists every container, network and volume on host, whichever
// stack created them, for the UI's host graph.
func (c *Controller) HostResources(ctx context.Context, host string) (*hosts.Resources, error) {
	h := c.Hosts.Get(host)
	if h == nil {
		return nil, fmt.Errorf("host %q: %w", host, ErrNotFound)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := c.Hosts.Backend.Resources(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("listing resources on %s: %w", host, err)
	}
	return out, nil
}

// HostObject is an engine object on a host, whether or not an Application
// deployed it: its inspect output, folded and masked like an app object's.
type HostObject struct {
	Host string `json:"host"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Project is the Compose project that created it, if any; App is set when
	// that project is an Application deployed to the host.
	Project string `json:"project,omitempty"`
	App     string `json:"app,omitempty"`
	// Image is a container's image reference.
	Image string `json:"image,omitempty"`
	// Redacted is whether environment values were hidden.
	Redacted bool      `json:"redacted,omitempty"`
	Inspect  Inspected `json:"inspect"`
}

// HostObject inspects the container, network, volume or image name on host.
// Environment values are hidden when redact is set, and always for objects no
// Application deployed: composecd never saw their values, so it can't tell
// which are secret.
func (c *Controller) HostObject(ctx context.Context, host, kind, name string, redact bool) (*HostObject, error) {
	if !compose.LookupKinds[kind] {
		return nil, fmt.Errorf("kind %q: want container, image, network or volume: %w", kind, ErrInvalidObject)
	}
	if name == "" {
		return nil, fmt.Errorf("a %s name is required: %w", kind, ErrInvalidObject)
	}
	h := c.Hosts.Get(host)
	if h == nil {
		return nil, fmt.Errorf("host %q: %w", host, ErrNotFound)
	}
	defer h.Use()()
	got, err := c.live.inspect(ctx, h.Name()+"\x00"+kind+"\x00"+name, func(ctx context.Context) (map[string]interface{}, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return h.Inspect(ctx, kind, name)
	})
	if err != nil {
		return nil, fmt.Errorf("inspecting %s %s on %s: %w", kind, name, host, err)
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("%s %q on %s: %w", kind, name, host, ErrNotFound)
	}
	out := &HostObject{Host: host, Kind: kind, Name: name, Project: projectOf(got)}
	if out.Project != "" {
		if a, err := c.application(out.Project); err == nil && a.Spec.Destination.Host == host {
			out.App = out.Project
		}
	}
	// A copy: the cache is shared with admins and with app dialogs.
	got = deepCopy(got).(map[string]interface{})
	if cfg, ok := got["Config"].(map[string]interface{}); ok && kind == KindContainer {
		out.Image, _ = cfg["Image"].(string)
	}
	if redact || out.App == "" {
		redactEnv(got)
		_, out.Redacted = lookup(got, []string{"Config", "Env"})
	}
	if out.Inspect, err = foldInspect(name, kind, got); err != nil {
		return nil, err
	}
	if out.App != "" {
		out.Inspect.YAML, out.Inspect.Folded = c.Mask(out.App, out.Inspect.YAML), maskFolds(c, out.App, out.Inspect.Folded)
	}
	return out, nil
}

// projectOf is the Compose project label of a container (under Config) or a
// network or volume (top level). Images have none.
func projectOf(inspect map[string]interface{}) string {
	for _, p := range [][]string{{"Config", "Labels", labelComposeProject}, {"Labels", labelComposeProject}} {
		if v, ok := lookup(inspect, p); ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}
