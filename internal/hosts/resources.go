package hosts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// Resources are the containers, networks and volumes on an engine, whichever
// project (if any) created them.
type Resources struct {
	Containers []ResourceContainer `json:"containers"`
	Networks   []ResourceNetwork   `json:"networks"`
	Volumes    []ResourceVolume    `json:"volumes"`
}

// ResourceContainer is a container and what it is attached to.
type ResourceContainer struct {
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"`
	State    string `json:"state,omitempty"`
	Health   string `json:"health,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	Project  string `json:"project,omitempty"`
	Service  string `json:"service,omitempty"`
	// Networks and Volumes are the engine names of the networks it joins and
	// the named volumes it mounts.
	Networks []string `json:"networks,omitempty"`
	Volumes  []string `json:"volumes,omitempty"`
}

// ResourceNetwork is a network and the project that created it.
type ResourceNetwork struct {
	Name    string `json:"name"`
	Driver  string `json:"driver,omitempty"`
	Project string `json:"project,omitempty"`
}

// ResourceVolume is a named volume and the project that created it.
type ResourceVolume struct {
	Name    string `json:"name"`
	Driver  string `json:"driver,omitempty"`
	Project string `json:"project,omitempty"`
}

const (
	projectLabel = "com.docker.compose.project"
	serviceLabel = "com.docker.compose.service"
)

// anonymous matches the names the engine gives anonymous volumes.
var anonymous = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Resources lists h's containers, networks and named volumes with one ls and
// one inspect each, so it costs six round trips however much runs there.
func (c *CLI) Resources(ctx context.Context, h *Host) (*Resources, error) {
	docker := func(args ...string) ([]byte, error) {
		var out, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "docker", append(h.Global(), args...)...)
		cmd.Env = append(os.Environ(), h.Env()...)
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
		}
		return out.Bytes(), nil
	}
	inspect := func(kind string, ls ...string) ([]byte, error) {
		out, err := docker(ls...)
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			return []byte("[]"), nil
		}
		return docker(append([]string{kind, "inspect", "--"}, ids...)...)
	}
	cts, err := inspect("container", "ps", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, err
	}
	nets, err := inspect("network", "network", "ls", "--quiet", "--no-trunc")
	if err != nil {
		return nil, err
	}
	vols, err := inspect("volume", "volume", "ls", "--quiet")
	if err != nil {
		return nil, err
	}
	return ParseResources(cts, nets, vols)
}

// ParseResources reads docker container, network and volume inspect output.
// Anonymous volumes and the host and none networks are left out.
func ParseResources(containers, networks, volumes []byte) (*Resources, error) {
	var cts []struct {
		Name   string
		Config struct {
			Image  string
			Labels map[string]string
		}
		State struct {
			Status   string
			ExitCode int
			Health   *struct{ Status string }
		}
		NetworkSettings struct {
			Networks map[string]interface{}
		}
		Mounts []struct {
			Type string
			Name string
		}
	}
	var nets, vols []struct {
		Name   string
		Driver string
		Labels map[string]string
	}
	if err := json.Unmarshal(containers, &cts); err != nil {
		return nil, fmt.Errorf("parsing docker container inspect output: %w", err)
	}
	if err := json.Unmarshal(networks, &nets); err != nil {
		return nil, fmt.Errorf("parsing docker network inspect output: %w", err)
	}
	if err := json.Unmarshal(volumes, &vols); err != nil {
		return nil, fmt.Errorf("parsing docker volume inspect output: %w", err)
	}
	out := &Resources{Containers: []ResourceContainer{}, Networks: []ResourceNetwork{}, Volumes: []ResourceVolume{}}
	for _, n := range nets {
		if n.Name == "host" || n.Name == "none" {
			continue
		}
		out.Networks = append(out.Networks, ResourceNetwork{Name: n.Name, Driver: n.Driver, Project: n.Labels[projectLabel]})
	}
	for _, v := range vols {
		if anonymous.MatchString(v.Name) {
			continue
		}
		out.Volumes = append(out.Volumes, ResourceVolume{Name: v.Name, Driver: v.Driver, Project: v.Labels[projectLabel]})
	}
	for _, c := range cts {
		r := ResourceContainer{Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image, State: c.State.Status, ExitCode: c.State.ExitCode,
			Project: c.Config.Labels[projectLabel], Service: c.Config.Labels[serviceLabel]}
		if c.State.Health != nil {
			r.Health = c.State.Health.Status
		}
		for n := range c.NetworkSettings.Networks {
			if n != "host" && n != "none" {
				r.Networks = append(r.Networks, n)
			}
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && !anonymous.MatchString(m.Name) {
				r.Volumes = append(r.Volumes, m.Name)
			}
		}
		sort.Strings(r.Networks)
		sort.Strings(r.Volumes)
		out.Containers = append(out.Containers, r)
	}
	sort.Slice(out.Containers, func(i, j int) bool { return out.Containers[i].Name < out.Containers[j].Name })
	sort.Slice(out.Networks, func(i, j int) bool { return out.Networks[i].Name < out.Networks[j].Name })
	sort.Slice(out.Volumes, func(i, j int) bool { return out.Volumes[i].Name < out.Volumes[j].Name })
	return out, nil
}
