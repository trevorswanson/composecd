// Package health assesses an Application from its live containers, with the
// statuses Argo CD uses.
package health

import (
	"fmt"
	"sort"
	"strings"

	"github.com/trevorswanson/composetpl/pkg/compose"
)

// Status is an application's health.
type Status string

const (
	Healthy     Status = "Healthy"
	Progressing Status = "Progressing"
	Degraded    Status = "Degraded"
	Suspended   Status = "Suspended"
	Missing     Status = "Missing"
	Unknown     Status = "Unknown"
)

// rank orders statuses from best to worst; the worst container wins.
var rank = map[Status]int{Healthy: 0, Suspended: 1, Progressing: 2, Missing: 3, Degraded: 4, Unknown: 5}

// Assess returns the health of a deployed release from its containers, with a
// message naming the containers that made it so. deployed is false when the
// release has no deployed revision.
func Assess(deployed bool, containers []compose.Container) (Status, string) {
	if !deployed {
		return Missing, "not deployed"
	}
	if len(containers) == 0 {
		return Missing, "no containers"
	}
	worst := Healthy
	reasons := map[Status][]string{}
	for _, c := range containers {
		s, why := Container(c)
		if s != Healthy {
			reasons[s] = append(reasons[s], fmt.Sprintf("%s %s", c.Name, why))
		}
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	msgs := reasons[worst]
	sort.Strings(msgs)
	return worst, strings.Join(msgs, "; ")
}

// Container returns one container's health and, unless it is healthy, why.
func Container(c compose.Container) (Status, string) {
	switch c.State {
	case "running":
		switch c.Health {
		case "unhealthy":
			return Degraded, "is unhealthy"
		case "starting":
			return Progressing, "is starting"
		}
		return Healthy, ""
	case "exited":
		if c.ExitCode != 0 {
			return Degraded, fmt.Sprintf("exited with code %d", c.ExitCode)
		}
		// One-shot services (init jobs) finish with 0.
		return Healthy, ""
	case "paused":
		return Suspended, "is paused"
	case "created":
		return Progressing, "is created but not started"
	case "restarting":
		return Degraded, "is restarting"
	case "dead", "removing":
		return Degraded, "is " + c.State
	}
	return Unknown, fmt.Sprintf("is in state %q", c.State)
}
