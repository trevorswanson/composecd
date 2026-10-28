package hosts

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func (c *CLI) Logs(ctx context.Context, h *Host, container string, tail int, follow bool, w io.Writer) error {
	if container == "" || strings.HasPrefix(container, "-") || tail < 0 || tail > 10000 {
		return fmt.Errorf("invalid container or tail (0–10000)")
	}
	args := append(h.Global(), "logs", "--tail", strconv.Itoa(tail))
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--", container)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), h.Env()...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker logs: %w", err)
	}
	return nil
}
