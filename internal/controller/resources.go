package controller

import (
	"context"
	"fmt"
	"time"

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
