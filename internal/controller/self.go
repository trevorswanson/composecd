package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
)

// ErrSelf refuses an operation that would stop composecd's own container
// halfway through.
var ErrSelf = errors.New("this is composecd itself")

// selfBlocked is why automation and manual operations leave the self app alone
// when composecd can't start a helper to update itself.
const selfBlocked = "composecd can't update itself here, because it didn't find its own container: run composetpl upgrade on the host instead"

// labelComposeProject is the label compose puts on every container it creates.
const labelComposeProject = "com.docker.compose.project"

// containerPath matches a container's directory in a bind-mount source, such as
// /var/lib/docker/containers/<id>/hostname (Docker) or
// .../overlay-containers/<id>/userdata/hostname (Podman).
var containerPath = regexp.MustCompile(`containers/([0-9a-f]{64})/`)

// ContainerID returns the ID of the container composecd runs in, read from the
// files the engine bind-mounts into it, or "" outside a container.
func ContainerID() string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	return containerIDFrom(f)
}

func containerIDFrom(r io.Reader) string {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		if m := containerPath.FindStringSubmatch(s.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

// detectSelf finds, on each host that is up, the compose project of
// composecd's own container: the self app's name on that host. A host that
// doesn't run our container gets "". A host that is down, or that couldn't be
// asked, keeps what was found before. Applications with spec.self are
// remembered for the life of the process, so prune keeps protecting them after
// they leave Git.
func (c *Controller) detectSelf(ctx context.Context, objs *config.Objects) {
	found := map[string]string{}
	if id := c.SelfContainer; id != "" {
		for _, h := range c.Hosts.All() {
			var err error
			if h.Status().State == hosts.StateUp {
				var obj map[string]interface{}
				if obj, err = h.Inspect(ctx, "container", id); err == nil {
					found[h.Name()] = composeProject(obj)
					if img, _ := obj["Image"].(string); img != "" {
						c.mu.Lock()
						c.selfImage = img
						c.mu.Unlock()
					}
					continue
				}
				c.Log.Debug("looking for composecd's own container", "host", h.Name(), "error", err)
			}
			c.mu.RLock()
			if p, ok := c.selfFound[h.Name()]; ok {
				found[h.Name()] = p
			}
			c.mu.RUnlock()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selfFound = found
	if c.selfPinned == nil {
		c.selfPinned = map[string]string{}
	}
	for _, a := range objs.Applications {
		if a.Spec.Self {
			c.selfPinned[a.Spec.Destination.Host] = a.Metadata.Name
		}
	}
}

// composeProject is the compose project label in `docker container inspect`
// output, or "".
func composeProject(obj map[string]interface{}) string {
	cfg, _ := obj["Config"].(map[string]interface{})
	labels, _ := cfg["Labels"].(map[string]interface{})
	p, _ := labels[labelComposeProject].(string)
	return p
}

// isSelf reports whether release name on host is composecd's own.
func (c *Controller) isSelf(host, name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isSelfLocked(host, name)
}

func (c *Controller) isSelfLocked(host, name string) bool {
	return name != "" && (c.selfFound[host] == name || c.selfPinned[host] == name)
}

func (c *Controller) isSelfApp(app *config.Application) bool {
	return c.isSelf(app.Spec.Destination.Host, app.Metadata.Name)
}

// selfConflict explains why app's spec.self contradicts what composecd found
// on its host, or returns "".
func (c *Controller) selfConflict(app *config.Application) string {
	if !app.Spec.Self {
		return ""
	}
	c.mu.RLock()
	p, ok := c.selfFound[app.Spec.Destination.Host]
	c.mu.RUnlock()
	if !ok || p == "" || p == app.Metadata.Name {
		return ""
	}
	return fmt.Sprintf("spec.self is set, but composecd runs as compose project %q on host %s", p, app.Spec.Destination.Host)
}

// selfRelease is the release composecd runs as on host, or "".
func (c *Controller) selfRelease(host string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p := c.selfFound[host]; p != "" {
		return p
	}
	return c.selfPinned[host]
}
