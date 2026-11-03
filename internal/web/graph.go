package web

import (
	"net/url"
	"sort"

	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
)

// graphNode is an object as graph.js draws it, without its YAML.
type graphNode struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Sub    string `json:"sub,omitempty"`
	Parent string `json:"parent,omitempty"`
	Sync   string `json:"sync,omitempty"`
	Health string `json:"health,omitempty"`
	// Dialog is the URL of the object's dialog; a node without one is only
	// shown.
	Dialog    string   `json:"dialog,omitempty"`
	Message   string   `json:"message,omitempty"`
	Uses      []string `json:"uses,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	// Stack is the ID of the stack box the host graph draws the node in.
	Stack string `json:"stack,omitempty"`
	// Neighbor nodes are another stack's, drawn on an app's graph because
	// they share one of its networks or volumes.
	Neighbor bool `json:"neighbor,omitempty"`
}

// graphStack is a box on the host graph: a Compose project, and the
// Application it is when composecd deploys it.
type graphStack struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Href string `json:"href,omitempty"`
}

// graph is what graph.js lays out. Key keeps its view (pan, zoom, filter)
// across live updates.
type graph struct {
	Key    string       `json:"key"`
	Nodes  []graphNode  `json:"nodes"`
	Stacks []graphStack `json:"stacks,omitempty"`
}

func objectDialog(app, id string) string {
	return "/ui/apps/" + url.PathEscape(app) + "/objects/" + id
}

// appGraph is an app's objects and, from its host's resources (nil if not
// listed yet), the other stacks' containers on its networks and volumes.
func appGraph(a controller.AppStatus, res *hosts.Resources, apps []controller.AppStatus) graph {
	nodes := make([]graphNode, 0, len(a.Objects))
	shared := map[string]string{} // engine name of a network or volume -> its object
	for _, o := range a.Objects {
		n := graphNode{ID: o.ID, Kind: o.Kind, Name: o.Name, Sub: o.Image, Parent: o.Parent, Sync: string(o.Sync),
			Health: string(o.Health), Message: o.Message, Uses: o.Uses, DependsOn: o.DependsOn, Dialog: objectDialog(a.Name, o.ID)}
		if n.Sub == "" && o.Kind != controller.KindContainer && o.LiveName != "" {
			n.Sub = o.LiveName
		}
		if (o.Kind == controller.KindNetwork || o.Kind == controller.KindVolume) && o.LiveName != "" {
			shared[o.Kind+"/"+o.LiveName] = o.ID
		}
		nodes = append(nodes, n)
	}
	if res != nil {
		managed := managedOn(a.Host, apps)
		for _, ct := range res.Containers {
			if ct.Project == a.Name {
				continue
			}
			var uses []string
			for _, n := range ct.Networks {
				if id := shared[controller.KindNetwork+"/"+n]; id != "" {
					uses = append(uses, id)
				}
			}
			for _, v := range ct.Volumes {
				if id := shared[controller.KindVolume+"/"+v]; id != "" {
					uses = append(uses, id)
				}
			}
			if len(uses) == 0 {
				continue
			}
			n := containerNode(ct, managed)
			n.ID, n.Uses, n.Neighbor = "neighbors/"+ct.Name, uses, true
			n.Sub = stackLabel(ct.Project, managed)
			nodes = append(nodes, n)
		}
	}
	return graph{Key: "app/" + a.Name, Nodes: nodes}
}

// managedOn maps each Application deployed to host to its objects' IDs by
// engine name ("network/web_default"), so a host graph can open their dialogs.
func managedOn(host string, apps []controller.AppStatus) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, a := range apps {
		if a.Host != host {
			continue
		}
		ids := map[string]string{}
		for _, o := range a.Objects {
			if o.LiveName != "" {
				ids[o.Kind+"/"+o.LiveName] = o.ID
			}
		}
		out[a.Name] = ids
	}
	return out
}

func stackLabel(project string, managed map[string]map[string]string) string {
	switch {
	case project == "":
		return "no stack"
	case managed[project] != nil:
		return "app " + project
	}
	return "stack " + project
}

func containerNode(ct hosts.ResourceContainer, managed map[string]map[string]string) graphNode {
	st, msg := health.Container(compose.Container{Name: ct.Name, State: ct.State, Health: ct.Health, ExitCode: ct.ExitCode})
	if msg == "" {
		msg = ct.State
	}
	n := graphNode{ID: "containers/" + ct.Name, Kind: controller.KindContainer, Name: ct.Name, Sub: ct.Image,
		Health: string(st), Message: ct.Image + " · " + msg}
	if ids := managed[ct.Project]; ids != nil && ids[controller.KindContainer+"/"+ct.Name] != "" {
		n.Dialog = objectDialog(ct.Project, ids[controller.KindContainer+"/"+ct.Name])
	}
	return n
}

// hostGraph is everything on a host, each container, network and volume in
// the box of the stack that created it.
func hostGraph(host string, res *hosts.Resources, apps []controller.AppStatus) graph {
	managed := managedOn(host, apps)
	g := graph{Key: "host/" + host, Nodes: []graphNode{}, Stacks: []graphStack{}}
	stacks := map[string]bool{}
	stack := func(project string) string {
		if project == "" {
			return ""
		}
		if !stacks[project] {
			stacks[project] = true
			s := graphStack{ID: "stacks/" + project, Name: project, Kind: "Compose project"}
			if managed[project] != nil {
				s.Kind, s.Href = "Application", "/apps/"+url.PathEscape(project)
			}
			g.Stacks = append(g.Stacks, s)
		}
		return "stacks/" + project
	}
	other := func(kind, name, driver, project string) graphNode {
		n := graphNode{ID: kind + "s/" + name, Kind: kind, Name: name, Sub: driver, Stack: stack(project)}
		if id := managed[project][kind+"/"+name]; id != "" {
			n.Dialog = objectDialog(project, id)
		}
		return n
	}
	for _, n := range res.Networks {
		g.Nodes = append(g.Nodes, other(controller.KindNetwork, n.Name, n.Driver, n.Project))
	}
	for _, v := range res.Volumes {
		g.Nodes = append(g.Nodes, other(controller.KindVolume, v.Name, v.Driver, v.Project))
	}
	for _, ct := range res.Containers {
		n := containerNode(ct, managed)
		n.Stack = stack(ct.Project)
		for _, net := range ct.Networks {
			n.Uses = append(n.Uses, "networks/"+net)
		}
		for _, v := range ct.Volumes {
			n.Uses = append(n.Uses, "volumes/"+v)
		}
		g.Nodes = append(g.Nodes, n)
	}
	sort.Slice(g.Stacks, func(i, j int) bool { return g.Stacks[i].Name < g.Stacks[j].Name })
	return g
}

// neighbors counts the nodes an app's graph shows only on request.
func (g graph) Neighbors() int {
	n := 0
	for _, node := range g.Nodes {
		if node.Neighbor {
			n++
		}
	}
	return n
}
