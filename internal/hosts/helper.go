package hosts

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// HelperSpec is a container that applies composecd's own release, which
// composecd can't do from inside its own container (docs/SELF.md).
type HelperSpec struct {
	Name  string
	Image string
	// Args follow the image's entrypoint.
	Args []string
	// VolumesFrom is composecd's own container, whose volumes (the Docker
	// socket, the data directory) the helper shares.
	VolumesFrom string
	Labels      map[string]string
	// Env is the helper's environment, as KEY=VALUE.
	Env []string
}

// StartHelper starts spec in the background on h.
func (c *CLI) StartHelper(ctx context.Context, h *Host, spec HelperSpec) error {
	args := append(h.Global(), "run", "--detach", "--name", spec.Name)
	if spec.VolumesFrom != "" {
		args = append(args, "--volumes-from", spec.VolumesFrom)
	}
	keys := make([]string, 0, len(spec.Labels))
	for k := range spec.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	for _, e := range spec.Env {
		args = append(args, "--env", e)
	}
	args = append(append(args, "--", spec.Image), spec.Args...)
	if _, err := c.docker(ctx, h, args...); err != nil {
		return fmt.Errorf("starting helper container %s: %w", spec.Name, err)
	}
	return nil
}

// WaitHelper blocks until container name on h exits, and returns its exit code.
func (c *CLI) WaitHelper(ctx context.Context, h *Host, name string) (int, error) {
	out, err := c.docker(ctx, h, append(h.Global(), "wait", "--", name)...)
	if err != nil {
		return 0, fmt.Errorf("waiting for helper container %s: %w", name, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("docker wait %s: unexpected output %q", name, out)
	}
	return code, nil
}

func (c *CLI) docker(ctx context.Context, h *Host, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), h.Env()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
