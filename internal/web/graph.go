package web

import (
	"net/url"
	"sort"
	"strings"

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
	// Stack is the ID of the stack box the host graph draws the node in;
	// shared nodes are drawn between boxes, and Owner is the stack that
	// created one.
	Stack  string `json:"stack,omitempty"`
	Shared bool   `json:"shared,omitempty"`
	Owner  string `json:"owner,omitempty"`
	// Hub is a shared node so many stacks use that the host graph draws its
	// edges only for what is pointed at.
	Hub bool `json:"hub,omitempty"`
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
	// Hubs are the hub nodes its containers use.
	Hubs []string `json:"hubs,omitempty"`
}

// graphCluster is stacks the host graph lays out together, because they
// share the networks or volumes in Shared.
type graphCluster struct {
	Stacks []string `json:"stacks"`
	Shared []string `json:"shared,omitempty"`
}

// graph is what graph.js lays out. Key keeps its view (pan, zoom, filter)
// across live updates.
type graph struct {
	Key    string       `json:"key"`
	Nodes  []graphNode  `json:"nodes"`
	Stacks []graphStack `json:"stacks,omitempty"`
	// Clusters order a host graph's stacks: the biggest groups of stacks
	// sharing networks or volumes first, then lone stacks, biggest first.
	Clusters []graphCluster `json:"clusters,omitempty"`
}

func objectDialog(app, id string) string {
	return "/ui/apps/" + url.PathEscape(app) + "/objects/" + id
}

// hostObjectDialog is the dialog of an engine object no Application owns.
func hostObjectDialog(host, kind, name string) string {
	return "/ui/hosts/" + url.PathEscape(host) + "/objects/" + kind + "/" + url.PathEscape(name)
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
			if n.Dialog == "" {
				n.Dialog = hostObjectDialog(a.Host, controller.KindContainer, ct.Name)
			}
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

// noStack is the box of objects no Compose project created.
const noStack = "stacks/"

// hubUsers is how many stacks may share a network or volume before it is a
// hub, whose edges would cross the whole host graph.
const hubUsers = 6

// hostGraph is everything on a host: each container, network and volume in
// the box of the stack that created it (objects no stack created share one
// box), and the networks and volumes stacks share between the boxes, with the
// stacks clustered by what they share.
func hostGraph(host string, res *hosts.Resources, apps []controller.AppStatus) graph {
	managed := managedOn(host, apps)
	g := graph{Key: "host/" + host, Nodes: []graphNode{}, Stacks: []graphStack{}, Clusters: []graphCluster{}}
	box := func(project string) string { return noStack + project }
	other := func(kind, name, driver, project string) graphNode {
		n := graphNode{ID: kind + "s/" + name, Kind: kind, Name: name, Sub: driver, Stack: box(project),
			Dialog: hostObjectDialog(host, kind, name)}
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
	// users are the boxes whose containers use each network or volume.
	users := map[string]map[string]bool{}
	for _, ct := range res.Containers {
		n := containerNode(ct, managed)
		if n.Dialog == "" {
			n.Dialog = hostObjectDialog(host, controller.KindContainer, ct.Name)
		}
		n.Stack = box(ct.Project)
		for _, net := range ct.Networks {
			n.Uses = append(n.Uses, "networks/"+net)
		}
		for _, v := range ct.Volumes {
			n.Uses = append(n.Uses, "volumes/"+v)
		}
		for _, u := range n.Uses {
			if users[u] == nil {
				users[u] = map[string]bool{}
			}
			users[u][n.Stack] = true
		}
		g.Nodes = append(g.Nodes, n)
	}

	// A network or volume used from more than one box moves out of them; one
	// no stack created that only one stack uses moves into that stack's box.
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Kind == controller.KindContainer {
			continue
		}
		by := users[n.ID]
		switch {
		case len(by) == 1 && n.Stack == noStack:
			for s := range by {
				n.Stack = s
			}
		case len(by) > 1 || (len(by) == 1 && !by[n.Stack]):
			if n.Stack != noStack {
				n.Owner = n.Stack
			}
			n.Stack, n.Shared, n.Hub = "", true, len(by) > hubUsers
		}
	}

	// Boxes are clustered by the shared nodes they use, hubs aside.
	parent := map[string]string{}
	var find func(string) string
	find = func(s string) string {
		if parent[s] == s {
			return s
		}
		parent[s] = find(parent[s])
		return parent[s]
	}
	size := map[string]int{}
	for _, n := range g.Nodes {
		if n.Stack != "" {
			parent[n.Stack] = n.Stack
			size[n.Stack]++
		}
	}
	hubs := map[string][]string{}
	for _, n := range g.Nodes {
		if !n.Shared {
			continue
		}
		var first string
		for s := range users[n.ID] {
			if n.Hub {
				hubs[s] = append(hubs[s], n.ID)
				continue
			}
			if first == "" {
				first = find(s)
			} else if r := find(s); r != first {
				parent[r] = first
			}
		}
	}
	clusters := map[string]*graphCluster{}
	for s := range parent {
		r := find(s)
		if clusters[r] == nil {
			clusters[r] = &graphCluster{}
		}
		clusters[r].Stacks = append(clusters[r].Stacks, s)
	}
	for _, n := range g.Nodes {
		if n.Shared && !n.Hub {
			for s := range users[n.ID] {
				c := clusters[find(s)]
				c.Shared = append(c.Shared, n.ID)
				break
			}
		}
	}
	weight := func(c *graphCluster) int {
		w := len(c.Shared)
		for _, s := range c.Stacks {
			w += size[s]
		}
		return w
	}
	for _, c := range clusters {
		sort.Strings(c.Stacks)
		sort.Strings(c.Shared)
		g.Clusters = append(g.Clusters, *c)
	}
	sort.Slice(g.Clusters, func(i, j int) bool {
		a, b := &g.Clusters[i], &g.Clusters[j]
		if len(a.Stacks) != len(b.Stacks) {
			return len(a.Stacks) > len(b.Stacks)
		}
		if wa, wb := weight(a), weight(b); wa != wb {
			return wa > wb
		}
		return a.Stacks[0] < b.Stacks[0]
	})

	for s := range parent {
		st := graphStack{ID: s, Name: strings.TrimPrefix(s, noStack), Kind: "Compose project", Hubs: hubs[s]}
		switch {
		case s == noStack:
			st.Name, st.Kind = "No stack", "Standalone"
		case managed[st.Name] != nil:
			st.Kind, st.Href = "Application", "/apps/"+url.PathEscape(st.Name)
		}
		sort.Strings(st.Hubs)
		g.Stacks = append(g.Stacks, st)
	}
	sort.Slice(g.Stacks, func(i, j int) bool {
		if (g.Stacks[i].ID == noStack) != (g.Stacks[j].ID == noStack) {
			return g.Stacks[j].ID == noStack
		}
		return g.Stacks[i].Name < g.Stacks[j].Name
	})
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
