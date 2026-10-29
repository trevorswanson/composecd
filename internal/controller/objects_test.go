package controller

import (
	"testing"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/health"
)

func TestBuildObjects(t *testing.T) {
	deployed := `name: app
services:
  web: {image: nginx:1, networks: [front], volumes: ["data:/data", "./conf:/etc/conf"], depends_on: {db: {condition: service_healthy}}, secrets: [token]}
  db: {image: postgres:16, volumes: [{type: volume, source: pg, target: /var/lib/postgresql}]}
  old: {image: busybox}
networks:
  front: {}
volumes:
  data: {}
  pg: {}
secrets:
  token: {file: ./token}
`
	desired := `name: app
services:
  web: {image: nginx:2, networks: [front], volumes: ["data:/data", "./conf:/etc/conf"], depends_on: {db: {condition: service_healthy}}, secrets: [token]}
  db: {image: postgres:16, volumes: [{type: volume, source: pg, target: /var/lib/postgresql}]}
  cache: {image: redis:8}
networks:
  front: {}
volumes:
  data: {}
  pg: {}
secrets:
  token: {file: ./token}
`
	changes := []Change{{Resource: "services/cache", Op: "added"}, {Resource: "services/old", Op: "removed"}, {Resource: "services/web", Op: "changed"}}
	containers := []compose.Container{
		{Name: "app-web-1", Service: "web", State: "running", Image: "nginx:1"},
		{Name: "app-old-1", Service: "old", State: "running"},
		{Name: "app-stray-1", Service: "stray", State: "exited", ExitCode: 1},
	}
	drift := []action.ServiceDrift{{Service: "db", Reason: action.DriftMissing}, {Service: "stray", Reason: action.DriftOrphaned, Containers: []string{"app-stray-1"}}}
	live := map[string]bool{"network/app_front": true, "volume/app_data": true}
	objs, err := buildObjects(objectInput{project: "app", desired: desired, deployed: deployed, changes: changes, containers: containers, drift: drift, live: true,
		inspect: func(kind, name string) (map[string]interface{}, error) {
			if live[kind+"/"+name] {
				return map[string]interface{}{"Name": name}, nil
			}
			return map[string]interface{}{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Object{}
	for _, o := range objs {
		got[o.ID] = o
	}
	want := map[string]ObjectSync{
		"services/web": ObjectOutOfSync, "services/db": ObjectMissing, "services/cache": ObjectMissing, "services/old": ObjectPrune,
		"services/stray": ObjectPrune, "networks/front": ObjectSynced, "volumes/data": ObjectSynced, "volumes/pg": ObjectMissing,
		"secrets/token": ObjectSynced, "networks/default": ObjectMissing,
		"containers/app-web-1": ObjectSynced, "containers/app-old-1": ObjectPrune, "containers/app-stray-1": ObjectPrune,
	}
	for id, sync := range want {
		if got[id].Sync != sync {
			t.Errorf("%s: sync %q, want %q", id, got[id].Sync, sync)
		}
	}
	if len(got) != len(want) {
		t.Errorf("objects = %v", objs)
	}
	web := got["services/web"]
	if web.Image != "nginx:2" || len(web.Uses) != 3 || web.Uses[0] != "networks/front" || web.Uses[1] != "secrets/token" || web.Uses[2] != "volumes/data" ||
		len(web.DependsOn) != 1 || web.DependsOn[0] != "services/db" || web.Health != health.Healthy {
		t.Errorf("web = %+v", web)
	}
	if db := got["services/db"]; len(db.Uses) != 2 || db.Uses[0] != "networks/default" || db.Uses[1] != "volumes/pg" || db.Health != health.Missing {
		t.Errorf("db = %+v", db)
	}
	if c := got["containers/app-stray-1"]; c.Parent != "services/stray" || c.Health != health.Degraded {
		t.Errorf("stray container = %+v", c)
	}
	if got["networks/front"].LiveName != "app_front" {
		t.Errorf("front = %+v", got["networks/front"])
	}
}
