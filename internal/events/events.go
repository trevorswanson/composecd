// Package events streams container events from a Docker engine with
// `docker events`, reporting which compose projects changed.
package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// actions that can change an app's health or drift.
var actions = map[string]bool{
	"create": true, "start": true, "restart": true, "die": true, "stop": true, "kill": true,
	"destroy": true, "pause": true, "unpause": true, "oom": true,
}

type event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// Project returns the compose project a `docker events` JSON line is about, if
// it is a lifecycle or health event of a (non one-off) compose container.
func Project(line []byte) (string, bool) {
	var e event
	if err := json.Unmarshal(line, &e); err != nil || e.Type != "container" {
		return "", false
	}
	if !actions[e.Action] && !strings.HasPrefix(e.Action, "health_status") {
		return "", false
	}
	attrs := e.Actor.Attributes
	// Hook runs (`docker compose run`) are one-off containers.
	if strings.EqualFold(attrs["com.docker.compose.oneoff"], "true") {
		return "", false
	}
	p := attrs["com.docker.compose.project"]
	return p, p != ""
}

// Watch runs `docker <global> events` with env added to the environment and
// calls on with the project of each relevant event, until ctx is done or the
// stream ends (for example because the engine went away).
func Watch(ctx context.Context, global, env []string, on func(project string)) error {
	args := append(append([]string{}, global...), "events", "--format", "{{json .}}",
		"--filter", "type=container", "--filter", "label=com.docker.compose.project")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	scanErr := Scan(stdout, on)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("docker events: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if scanErr != nil {
		return scanErr
	}
	return fmt.Errorf("docker events: the stream ended")
}

// Scan reads `docker events` JSON lines from r and calls on for each relevant one.
func Scan(r io.Reader, on func(project string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if p, ok := Project(sc.Bytes()); ok {
			on(p)
		}
	}
	return sc.Err()
}
