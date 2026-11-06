package web

import (
	"reflect"
	"testing"

	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/hosts"
)

var shared = &hosts.Resources{
	Containers: []hosts.ResourceContainer{
		{Name: "web-app-1", Image: "nginx", State: "running", Project: "web", Networks: []string{"proxy", "web_default"}},
		{Name: "traefik-proxy-1", Image: "traefik", State: "running", Project: "traefik", Networks: []string{"proxy"}},
		{Name: "loose", Image: "busybox", State: "exited", Networks: []string{"bridge"}},
	},
	Networks: []hosts.ResourceNetwork{{Name: "bridge"}, {Name: "proxy", Project: "traefik"}, {Name: "web_default", Project: "web"}},
}

var web = controller.AppStatus{Name: "web", Host: "nas", Objects: []controller.Object{
	{ID: "containers/web-app-1", Kind: controller.KindContainer, Name: "web-app-1", LiveName: "web-app-1", Parent: "services/app"},
	{ID: "networks/default", Kind: controller.KindNetwork, Name: "default", LiveName: "web_default"},
	{ID: "networks/proxy", Kind: controller.KindNetwork, Name: "proxy", LiveName: "proxy", External: true},
	{ID: "services/app", Kind: controller.KindService, Name: "app", Uses: []string{"networks/default", "networks/proxy"}},
}}

func TestAppGraphNeighbors(t *testing.T) {
	g := appGraph(web, shared, []controller.AppStatus{web})
	if g.Neighbors() != 1 {
		t.Fatalf("neighbors: %+v", g.Nodes)
	}
	n := g.Nodes[len(g.Nodes)-1]
	if n.ID != "neighbors/traefik-proxy-1" || !n.Neighbor || n.Sub != "stack traefik" || n.Dialog != "/ui/hosts/nas/objects/container/traefik-proxy-1" ||
		!reflect.DeepEqual(n.Uses, []string{"networks/proxy"}) {
		t.Fatalf("neighbor: %+v", n)
	}
	if g := appGraph(web, nil, nil); g.Neighbors() != 0 || len(g.Nodes) != 4 || g.Nodes[0].Dialog != "/ui/apps/web/objects/containers/web-app-1" {
		t.Fatalf("without resources: %+v", g.Nodes)
	}
}

func TestHostGraph(t *testing.T) {
	g := hostGraph("nas", shared, []controller.AppStatus{web, {Name: "other", Host: "elsewhere"}})
	want := []graphStack{{ID: "stacks/traefik", Name: "traefik", Kind: "Compose project"},
		{ID: "stacks/web", Name: "web", Kind: "Application", Href: "/apps/web"}, {ID: "stacks/", Name: "No stack", Kind: "Standalone"}}
	if !reflect.DeepEqual(g.Stacks, want) {
		t.Fatalf("stacks: %+v", g.Stacks)
	}
	by := map[string]graphNode{}
	for _, n := range g.Nodes {
		by[n.ID] = n
	}
	if n := by["containers/web-app-1"]; n.Stack != "stacks/web" || n.Dialog != "/ui/apps/web/objects/containers/web-app-1" ||
		!reflect.DeepEqual(n.Uses, []string{"networks/proxy", "networks/web_default"}) {
		t.Fatalf("app container: %+v", n)
	}
	if n := by["networks/web_default"]; n.Stack != "stacks/web" || n.Shared || n.Dialog != "/ui/apps/web/objects/networks/default" {
		t.Fatalf("app network: %+v", n)
	}
	// Used by two stacks, the proxy network moves out of the box of the stack
	// that created it, and opens as a host object: no app owns it.
	if n := by["networks/proxy"]; n.Stack != "" || !n.Shared || n.Hub || n.Owner != "stacks/traefik" ||
		n.Dialog != "/ui/hosts/nas/objects/network/proxy" {
		t.Fatalf("shared network: %+v", n)
	}
	if n := by["containers/loose"]; n.Stack != "stacks/" || n.Health == "" || n.Dialog != "/ui/hosts/nas/objects/container/loose" {
		t.Fatalf("loose container: %+v", n)
	}
	if n := by["networks/bridge"]; n.Stack != "stacks/" || n.Shared {
		t.Fatalf("bridge: %+v", n)
	}
	clusters := []graphCluster{{Stacks: []string{"stacks/traefik", "stacks/web"}, Shared: []string{"networks/proxy"}}, {Stacks: []string{"stacks/"}}}
	if !reflect.DeepEqual(g.Clusters, clusters) {
		t.Fatalf("clusters: %+v", g.Clusters)
	}
}

// A network most stacks join is a hub: it doesn't cluster them, and each
// stack lists it instead. Stacks sharing anything else cluster around it, the
// biggest cluster first.
func TestHostGraphClustersAndHubs(t *testing.T) {
	res := &hosts.Resources{Networks: []hosts.ResourceNetwork{{Name: "proxy", Project: "traefik"}, {Name: "db", Project: "pg"},
		{Name: "cache"}, {Name: "a b/c"}}}
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		res.Containers = append(res.Containers, hosts.ResourceContainer{Name: p + "-1", Project: p, State: "running", Networks: []string{"proxy"}})
	}
	res.Containers = append(res.Containers,
		hosts.ResourceContainer{Name: "pg-1", Project: "pg", State: "running", Networks: []string{"db"}},
		hosts.ResourceContainer{Name: "f-2", Project: "f", State: "running", Networks: []string{"db"}},
		hosts.ResourceContainer{Name: "g-2", Project: "g", State: "running", Networks: []string{"cache", "db"}},
		hosts.ResourceContainer{Name: "solo-1", Project: "solo", State: "exited", Networks: []string{"a b/c"}})
	g := hostGraph("nas", res, nil)
	by := map[string]graphNode{}
	for _, n := range g.Nodes {
		by[n.ID] = n
	}
	if n := by["networks/proxy"]; !n.Shared || !n.Hub || n.Owner != "stacks/traefik" {
		t.Fatalf("hub: %+v", n)
	}
	if n := by["networks/db"]; !n.Shared || n.Hub || n.Owner != "stacks/pg" {
		t.Fatalf("shared: %+v", n)
	}
	// A network no stack created moves into the box of the one stack using it.
	if n := by["networks/cache"]; n.Shared || n.Stack != "stacks/g" {
		t.Fatalf("cache: %+v", n)
	}
	if n := by["networks/a b/c"]; n.Stack != "stacks/solo" || n.Dialog != "/ui/hosts/nas/objects/network/a%20b%2Fc" {
		t.Fatalf("escaped: %+v", n)
	}
	if len(g.Clusters) != 7 || !reflect.DeepEqual(g.Clusters[0], graphCluster{Stacks: []string{"stacks/f", "stacks/g", "stacks/pg"},
		Shared: []string{"networks/db"}}) {
		t.Fatalf("clusters: %+v", g.Clusters)
	}
	// Lone stacks follow, biggest first, then by name.
	var rest []string
	for _, c := range g.Clusters[1:] {
		rest = append(rest, c.Stacks...)
	}
	if !reflect.DeepEqual(rest, []string{"stacks/solo", "stacks/a", "stacks/b", "stacks/c", "stacks/d", "stacks/e"}) {
		t.Fatalf("order: %v", rest)
	}
	for _, st := range g.Stacks {
		if st.ID == "stacks/traefik" {
			t.Fatal("a stack with nothing in its box has none")
		}
		if hub := st.ID != "stacks/pg" && st.ID != "stacks/solo"; hub != reflect.DeepEqual(st.Hubs, []string{"networks/proxy"}) {
			t.Fatalf("hubs of %s: %v", st.ID, st.Hubs)
		}
	}
}
