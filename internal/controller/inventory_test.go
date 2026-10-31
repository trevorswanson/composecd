package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/trevorswanson/composecd/internal/masking"
)

const dbTemplate = `services:
  db:
    image: postgres:16
    environment: {PASSWORD: hunter2, USER: app}
    labels: [gatus.enable=true]
    configs: [{source: init, target: /init.sql}]
configs:
  init: {content: select 1}
`

// readerTemplate puts what lookup returns where the test can read it back.
const readerTemplate = `services:
  gatus:
    image: gatus
    environment:
      ITEMS: {{ (lookup "composecd/application" "").items | toJson | quote }}
      ONE: {{ (lookup "composecd/application" "db").host | default "none" | quote }}
      NONE: {{ (lookup "composecd/application" "nope").host | default "none" | quote }}
`

// readmeTemplate is the README's example.
const readmeTemplate = `{{- range $app := (lookup "composecd/application" "").items }}
{{- range $svc, $s := $app.services }}
{{- if eq (index $s.labels "gatus.enable" | default "") "true" }}
# {{ $app.name }} on {{ $app.host }}: {{ $svc }}
{{- end }}
{{- end }}
{{- end }}
`

func lookupApps(perms string) string {
	app := func(name, chart, extra string) string {
		return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n" +
			"  source: {path: charts/" + chart + "}\n  destination: {host: nas}\n" +
			"  syncPolicy: {automated: {}, options: {wait: false}}\n" + extra
	}
	// gatus comes first so the cold start renders it before what it reads.
	return app("gatus", "gatus", perms) + "---\n" + app("web", "web", "") + "---\n" + app("db", "db", "")
}

func newLookupEnv(t *testing.T, perms string) *env {
	e := newEnv(t)
	e.commit(map[string]string{
		"composecd/apps.yaml":           lookupApps(perms),
		"charts/db/Chart.yaml":          "apiVersion: v2\nname: db\nversion: 0.1.0\n",
		"charts/db/templates/a.yaml":    dbTemplate,
		"charts/gatus/Chart.yaml":       "apiVersion: v2\nname: gatus\nversion: 0.1.0\n",
		"charts/gatus/templates/a.yaml": readerTemplate,
		"charts/gatus/templates/b.yaml": readmeTemplate,
	})
	return e
}

// deployedLookup reads back the reader's deployed lookup results.
func deployedLookup(t *testing.T, e *env) (items map[string]map[string]interface{}, one, none string) {
	t.Helper()
	var project struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(e.fake().Running["gatus"]), &project); err != nil {
		t.Fatal(err)
	}
	env := project.Services["gatus"].Environment
	var list []map[string]interface{}
	if err := json.Unmarshal([]byte(env["ITEMS"]), &list); err != nil {
		t.Fatalf("ITEMS %q: %v", env["ITEMS"], err)
	}
	items = map[string]map[string]interface{}{}
	for _, it := range list {
		items[it["name"].(string)] = it
	}
	return items, env["ONE"], env["NONE"]
}

func service(t *testing.T, items map[string]map[string]interface{}, app, svc string) map[string]interface{} {
	t.Helper()
	s, ok := items[app]["services"].(map[string]interface{})[svc].(map[string]interface{})
	if !ok {
		t.Fatalf("no service %s/%s in %v", app, svc, items[app])
	}
	return s
}

func TestLookupApplicationsLabelsOnly(t *testing.T) {
	e := newLookupEnv(t, "")
	e.refresh()
	if st := e.app("gatus"); st.Sync.Status != Synced || st.Error != "" {
		t.Fatalf("reader after the first refresh: %+v", st)
	}
	items, one, none := deployedLookup(t, e)
	if len(items) != 2 || items["gatus"] != nil || one != "nas" || none != "none" {
		t.Fatalf("items=%v one=%q none=%q", items, one, none)
	}
	db := service(t, items, "db", "db")
	if db["labels"].(map[string]interface{})["gatus.enable"] != "true" || db["image"] != "postgres:16" {
		t.Errorf("db service: %v", db)
	}
	for _, k := range []string{"environment", "configs", "secrets"} {
		if _, ok := db[k]; ok {
			t.Errorf("%s visible without a grant: %v", k, db)
		}
	}
	if items["web"]["host"] != "nas" || service(t, items, "web", "web")["labels"].(map[string]interface{})["tz"] != "UTC" {
		t.Errorf("web: %v", items["web"])
	}

	// A steady state: no resync, and the sync's render matched assess's.
	ups := e.ups("gatus")
	e.refresh()
	if st := e.app("gatus"); st.Sync.Status != Synced || e.ups("gatus") != ups {
		t.Fatalf("reader resynced: %+v ups=%d->%d", st.Sync, ups, e.ups("gatus"))
	}

	// An app added to Git shows up in the same refresh.
	e.commit(map[string]string{"composecd/apps.yaml": lookupApps("") + "---\n" + appYAML("extra", true, "")})
	e.refresh()
	if items, _, _ := deployedLookup(t, e); items["extra"] == nil {
		t.Fatalf("new app missing: %v", items)
	}
}

func TestLookupApplicationsPermissions(t *testing.T) {
	e := newLookupEnv(t, "  permissions: {env: ['db'], configs: ['db/db'], secrets: ['web/*']}\n")
	e.c.remember("db", []string{"hunter2"}) // as a decrypted SOPS value would be
	e.refresh()
	items, _, _ := deployedLookup(t, e)
	db := service(t, items, "db", "db")
	env := db["environment"].(map[string]interface{})
	if env["PASSWORD"] != masking.Replacement || env["USER"] != "app" {
		t.Errorf("env grant should mask secrets: %v", env)
	}
	if db["configs"].(map[string]interface{})["/init.sql"] != "select 1" {
		t.Errorf("configs: %v", db["configs"])
	}
	if _, ok := service(t, items, "web", "web")["environment"]; !ok {
		t.Error("secrets grant should include the environment")
	}

	e.commit(map[string]string{"composecd/apps.yaml": lookupApps("  permissions: {secrets: ['d*']}\n")})
	e.refresh()
	items, _, _ = deployedLookup(t, e)
	if got := service(t, items, "db", "db")["environment"].(map[string]interface{})["PASSWORD"]; got != "hunter2" {
		t.Errorf("secrets grant should unmask: %v", got)
	}
	if _, ok := service(t, items, "db", "db")["configs"]; ok {
		t.Error("configs visible without a configs grant")
	}
}

func TestLookupApplicationsFailsClosed(t *testing.T) {
	e := newLookupEnv(t, "")
	e.refresh()
	e.c.mu.Lock()
	e.c.apps["web"].known = false
	reader := e.c.objects.Application("gatus")
	e.c.mu.Unlock()
	if _, err := e.c.inventory(reader); err == nil || !strings.Contains(err.Error(), "not yet reconciled: web") {
		t.Fatalf("inventory with an unknown app: %v", err)
	}
	if !e.c.isReader("gatus") || e.c.isReader("web") {
		t.Error("only gatus reads other Applications")
	}
}
