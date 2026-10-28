package controller

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/trevorswanson/composecd/internal/source"
)

// Logs resolves a container from a fresh compose ps for this app. Never accept
// an arbitrary Docker name from a read-only client.
func (c *Controller) Logs(ctx context.Context, name, container string, tail int, follow bool, w io.Writer) error {
	if tail < 0 || tail > 10000 {
		return fmt.Errorf("tail must be between 0 and 10000")
	}
	app, err := c.application(name)
	if err != nil {
		return err
	}
	h := c.Hosts.Get(app.Spec.Destination.Host)
	if h == nil {
		return ErrNotFound
	}
	defer h.Use()()
	if _, err := c.releaseHistory(h, name); err != nil {
		return fmt.Errorf("reading release history: %w", err)
	}
	containers, err := h.Configuration(source.Env(h.Spec, app), io.Discard).Compose.PS(ctx, name)
	if err != nil {
		return err
	}
	for _, ct := range containers {
		if ct.Name == container && !strings.HasPrefix(ct.Name, "-") && ct.Labels["com.docker.compose.project"] == name {
			if starter, ok := w.(interface{ Start() }); ok {
				starter.Start()
			}
			return c.Hosts.Backend.Logs(ctx, h, ct.Name, tail, follow, w)
		}
	}
	return fmt.Errorf("container %q in application %q: %w", container, name, ErrNotFound)
}
