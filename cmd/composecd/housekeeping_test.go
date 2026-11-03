package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/trevorswanson/composecd/internal/hosts"
)

// prunes records the prunes the CLI asks for.
type prunes struct {
	mu    sync.Mutex
	calls []string
}

func (p *prunes) DiskUsage(context.Context, *hosts.Host) (*hosts.DiskUsage, error) {
	return &hosts.DiskUsage{Size: 100e9, Used: 90e9, Available: 10e9}, nil
}

func (p *prunes) PruneImages(_ context.Context, h *hosts.Host, o hosts.PruneOptions, w io.Writer) error {
	p.record(fmt.Sprintf("images %s all=%v until=%v", h.Name(), o.All, o.Until))
	fmt.Fprintln(w, "Total reclaimed space: 1.2GB")
	return nil
}

func (p *prunes) PruneBuildCache(_ context.Context, h *hosts.Host, o hosts.PruneOptions, w io.Writer) error {
	p.record(fmt.Sprintf("cache %s all=%v keep=%s", h.Name(), o.All, o.KeepStorage))
	fmt.Fprintln(w, "Total:\t3GB")
	return nil
}

func (p *prunes) record(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, s)
}

func TestHostPruneCLI(t *testing.T) {
	srv, c, _ := startServer(t)
	p := &prunes{}
	c.Housekeeper = p

	expect(t, mustCLI(t, srv, "host", "prune", "nas", "--logs"), "Total reclaimed space: 1.2GB", "Succeeded: reclaimed 1.2GB")
	expect(t, mustCLI(t, srv, "host", "prune", "nas", "--build-cache", "--all"), "Succeeded: reclaimed 3GB")
	if got := strings.Join(p.calls, "; "); got != "images nas all=false until=168h0m0s; cache nas all=true keep=10GB" {
		t.Errorf("prunes = %s", got)
	}
	expect(t, mustCLI(t, srv, "host", "operations", "nas"), "KIND", "builder-prune", "image-prune", "admin", "Succeeded", "reclaimed 3GB")
	expect(t, mustCLI(t, srv, "operation", "list"), "host nas", "image-prune")
	if _, err := cli(t, srv, "host", "prune", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown host: %v", err)
	}

	// A full disk shows in host list once a ping measures it.
	mustCLI(t, srv, "refresh")
	c.RefreshAll(context.Background())
	c.Wait()
	expect(t, mustCLI(t, srv, "host", "list"), "the disk is 90% full (warning at 85%)")
}
