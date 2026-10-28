package config

import (
	"strings"
	"testing"
	"time"
)

const p2host = "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\n---\n"

func p2app(name, extra string) string {
	return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n  source: {path: x}\n  destination: {host: nas}\n" + extra + "---\n"
}

func TestPhase2Fields(t *testing.T) {
	doc := p2host + p2app("db", "") + p2app("traefik", "") +
		p2app("media", "  dependsOn: [db, traefik]\n  syncPolicy:\n    automated: {prune: true, selfHeal: true}\n    retry: {limit: 3, backoff: {duration: 5s, factor: 3, maxDuration: 1m}}\n") +
		p2app("photos", "  dependsOn: [media]\n") + `apiVersion: composecd/v1alpha1
kind: SyncWindow
metadata: {name: nightly}
spec:
  kind: allow
  schedule: "0 2 * * *"
  duration: 3h
  timeZone: America/Chicago
  applications: ["*"]
  labels: {tier: apps}
  manualSync: true
`
	objs, err := Parse("t.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	media := objs.Application("media")
	if a := media.Spec.SyncPolicy.Automated; !a.Prune || !a.SelfHeal {
		t.Errorf("automated = %+v", a)
	}
	r := media.Spec.SyncPolicy.Retry
	if r.Limit != 3 || r.Backoff.Delay(1) != 5*time.Second || r.Backoff.Delay(2) != 15*time.Second ||
		r.Backoff.Delay(3) != 45*time.Second || r.Backoff.Delay(4) != time.Minute {
		t.Errorf("retry = %+v, delays %v %v %v %v", r, r.Backoff.Delay(1), r.Backoff.Delay(2), r.Backoff.Delay(3), r.Backoff.Delay(4))
	}
	if d := (Backoff{}).Delay(1); d != DefaultBackoff {
		t.Errorf("default delay = %v", d)
	}
	w := objs.SyncWindows[0]
	if w.Spec.Kind != WindowAllow || time.Duration(w.Spec.Duration) != 3*time.Hour || !w.Spec.ManualSync || w.Origin != "t.yaml:38" {
		t.Errorf("window = %+v", w)
	}

	var names [][]string
	for _, wave := range objs.Waves() {
		var ns []string
		for _, a := range wave {
			ns = append(ns, a.Metadata.Name)
		}
		names = append(names, ns)
	}
	if got := strings.Join([]string{strings.Join(names[0], ","), strings.Join(names[1], ","), strings.Join(names[2], ",")}, " | "); got != "db,traefik | media | photos" || len(names) != 3 {
		t.Errorf("waves = %v", names)
	}
}

func TestPhase2ValidationErrors(t *testing.T) {
	win := func(spec string) string {
		return "apiVersion: composecd/v1alpha1\nkind: SyncWindow\nmetadata: {name: w}\nspec: " + spec + "\n"
	}
	cases := map[string]string{
		"self dependency":    p2host + p2app("a", "  dependsOn: [a]\n"),
		"missing dependency": p2host + p2app("a", "  dependsOn: [nope]\n"),
		"cycle":              p2host + p2app("a", "  dependsOn: [b]\n") + p2app("b", "  dependsOn: [c]\n") + p2app("c", "  dependsOn: [a]\n"),
		"negative retry":     p2host + p2app("a", "  syncPolicy: {retry: {limit: -1}}\n"),
		"small factor":       p2host + p2app("a", "  syncPolicy: {retry: {limit: 1, backoff: {factor: 0.5}}}\n"),
		"window kind":        win(`{kind: maybe, schedule: "@daily", duration: 1h, applications: ["*"]}`),
		"window schedule":    win(`{kind: deny, schedule: "every night", duration: 1h, applications: ["*"]}`),
		"window duration":    win(`{kind: deny, schedule: "@daily", applications: ["*"]}`),
		"window zone":        win(`{kind: deny, schedule: "@daily", duration: 1h, timeZone: Mars/Base, applications: ["*"]}`),
		"window selector":    win(`{kind: deny, schedule: "@daily", duration: 1h}`),
		"window glob":        win(`{kind: deny, schedule: "@daily", duration: 1h, hosts: ["["]}`),
	}
	for name, doc := range cases {
		if _, err := Parse("t.yaml", []byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	_, err := Parse("t.yaml", []byte(cases["cycle"]))
	if err == nil || !strings.Contains(err.Error(), "a -> b -> c -> a") {
		t.Errorf("cycle error = %v", err)
	}
}

func TestBootstrapNotifications(t *testing.T) {
	b, err := ParseBootstrap([]byte(`
root: {repoURL: x}
webhook: {secretFile: /run/secrets/hook}
notifications:
  - {name: phone, type: ntfy, url: https://ntfy.sh/homelab, events: [sync-failed, self-healed]}
  - {name: chat, type: discord, urlFile: /run/secrets/discord, applications: ["media-*"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if b.Webhook.SecretFile != "/run/secrets/hook" || len(b.Notifications) != 2 || b.Notifications[1].URLFile == "" {
		t.Errorf("bootstrap = %+v", b)
	}
	for name, doc := range map[string]string{
		"no name":    "root: {repoURL: x}\nnotifications: [{type: ntfy, url: u}]\n",
		"bad type":   "root: {repoURL: x}\nnotifications: [{name: a, type: pager, url: u}]\n",
		"no url":     "root: {repoURL: x}\nnotifications: [{name: a, type: ntfy}]\n",
		"both urls":  "root: {repoURL: x}\nnotifications: [{name: a, type: ntfy, url: u, urlFile: f}]\n",
		"bad event":  "root: {repoURL: x}\nnotifications: [{name: a, type: ntfy, url: u, events: [explode]}]\n",
		"duplicated": "root: {repoURL: x}\nnotifications: [{name: a, type: ntfy, url: u}, {name: a, type: slack, url: u}]\n",
	} {
		if _, err := ParseBootstrap([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
