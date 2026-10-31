// Package events streams container and volume events from a Docker engine with
// `docker events`, reporting which compose projects changed and when
// composetpl's release storage was written.
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

	"github.com/trevorswanson/composetpl/pkg/storage"
)

// Handlers receive what an engine reports. Either may be nil.
type Handlers struct {
	// Project is called with the compose project of each container event that
	// can change an app's health or drift.
	Project func(project string)
	// Storage is called when another client may have written composetpl's
	// release volume, so cached release history is stale.
	Storage func()
}

// actions that can change an app's health or drift.
var actions = map[string]bool{
	"create": true, "start": true, "restart": true, "die": true, "stop": true, "kill": true,
	"destroy": true, "pause": true, "unpause": true, "oom": true,
}

type event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
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

// storageWrites tells, from a stream of events, when composetpl's release
// volume is written. composetpl reads and writes it with docker cp through a
// stopped accessor container, and deletes from it with a short-lived helper.
// Older clients use helpers for everything.
//
// A write through the accessor is an extract-to-dir event on it. A helper's
// write is a read-write mount of the volume by a container that then starts.
// Mounts alone don't tell: the accessor mounts the volume read-write for
// every docker cp, reads included, but never starts.
type storageWrites struct {
	volume  string          // empty means storage.DefaultVolume
	mounted map[string]bool // containers with the volume mounted read-write
}

func (w *storageWrites) written(e *event) bool {
	attrs := e.Actor.Attributes
	volume := w.volume
	if volume == "" {
		volume = storage.DefaultVolume
	}
	switch {
	case e.Type == "container" && e.Action == "extract-to-dir":
		return attrs[storage.Label] != ""
	case e.Type == "volume" && e.Actor.ID == volume:
		switch e.Action {
		case "mount":
			if attrs["read/write"] == "true" {
				if w.mounted == nil {
					w.mounted = map[string]bool{}
				}
				w.mounted[attrs["container"]] = true
			}
		case "unmount":
			delete(w.mounted, attrs["container"])
		}
	case e.Type == "container" && e.Action == "start" && w.mounted[e.Actor.ID]:
		delete(w.mounted, e.Actor.ID)
		return true
	}
	return false
}

// Watch runs `docker <global> events` with env added to the environment and
// calls on for each relevant event, until ctx is done or the stream ends (for
// example because the engine went away).
func Watch(ctx context.Context, global, env []string, on Handlers) error {
	// Filters on different keys are ANDed, so a label filter would hide volume
	// events: projects are picked out in Scan instead.
	args := append(append([]string{}, global...), "events", "--format", "{{json .}}",
		"--filter", "type=container", "--filter", "type=volume")
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
func Scan(r io.Reader, on Handlers) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var writes storageWrites
	for sc.Scan() {
		if p, ok := Project(sc.Bytes()); ok && on.Project != nil {
			on.Project(p)
		}
		var e event
		if json.Unmarshal(sc.Bytes(), &e) == nil && writes.written(&e) && on.Storage != nil {
			on.Storage()
		}
	}
	return sc.Err()
}
