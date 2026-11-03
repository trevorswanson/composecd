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
	if n.ID != "neighbors/traefik-proxy-1" || !n.Neighbor || n.Sub != "stack traefik" || n.Dialog != "" ||
		!reflect.DeepEqual(n.Uses, []string{"networks/proxy"}) {
		t.Fatalf("neighbor: %+v", n)
	}
	if g := appGraph(web, nil, nil); g.Neighbors() != 0 || len(g.Nodes) != 4 || g.Nodes[0].Dialog != "/ui/apps/web/objects/containers/web-app-1" {
		t.Fatalf("without resources: %+v", g.Nodes)
	}
}

func TestHostGraph(t *testing.T) {
	g := hostGraph("nas", shared, []controller.AppStatus{web, {Name: "other", Host: "elsewhere"}})
	want := []graphStack{{ID: "stacks/traefik", Name: "traefik", Kind: "Compose project"}, {ID: "stacks/web", Name: "web", Kind: "Application", Href: "/apps/web"}}
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
	if n := by["networks/web_default"]; n.Dialog != "/ui/apps/web/objects/networks/default" {
		t.Fatalf("app network: %+v", n)
	}
	if n := by["networks/proxy"]; n.Stack != "stacks/traefik" || n.Dialog != "" {
		t.Fatalf("shared network: %+v", n)
	}
	if n := by["containers/loose"]; n.Stack != "" || n.Health == "" {
		t.Fatalf("loose container: %+v", n)
	}
}
