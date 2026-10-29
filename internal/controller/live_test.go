package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

const inspectWeb = `{
  "Id": "4f1c0ffee", "Created": "2026-10-29T12:00:00Z", "Name": "/demo-web-1", "Image": "sha256:img",
  "GraphDriver": {"Name": "overlay2", "Data": {"LowerDir": "/x"}}, "LogPath": "/var/lib/docker/x.log",
  "State": {"Status": "running", "Health": {"Status": "healthy", "Log": [{"ExitCode": 0}]}},
  "Config": {"Image": "nginx:1.27", "Hostname": "4f1c0ffee", "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.27", "MODE=prod", "API_TOKEN=hunter2"],
    "Cmd": ["nginx", "-g", "daemon off;"], "Entrypoint": ["/docker-entrypoint.sh"], "WorkingDir": "",
    "Labels": {"com.docker.compose.project": "demo", "com.docker.compose.service": "web", "tier": "front", "maintainer": "NGINX"}},
  "HostConfig": {"RestartPolicy": {"Name": "unless-stopped"}, "PortBindings": {"80/tcp": [{"HostIp": "", "HostPort": "8080"}]},
    "Privileged": false, "CapAdd": null, "Dns": [], "Memory": 0},
  "Mounts": [{"Type": "volume", "Name": "demo_static", "Destination": "/usr/share/nginx/html", "RW": true},
    {"Type": "bind", "Source": "/opt/composecd/demo/conf", "Destination": "/etc/nginx/conf.d", "RW": false},
    {"Type": "volume", "Name": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "Destination": "/cache", "RW": true}],
  "NetworkSettings": {"SandboxKey": "/var/run/docker/netns/x", "Networks": {"demo_front": {}, "demo_back": {}}}
}`

const inspectImage = `{"Config": {"Env": ["PATH=/usr/bin", "NGINX_VERSION=1.27"], "Cmd": ["nginx", "-g", "daemon off;"],
  "Entrypoint": ["/docker-entrypoint.sh"], "Labels": {"maintainer": "NGINX"}}}`

const desiredWeb = `image: nginx:1.27
restart: unless-stopped
ports: ["8080:80"]
environment: [MODE=prod, API_TOKEN=hunter2]
labels: {tier: front}
networks: [front, back]
volumes: ["static:/usr/share/nginx/html", "./conf:/etc/nginx/conf.d:ro"]
depends_on: [cache]
`

func decode(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestProjectionMatchesDesired(t *testing.T) {
	live := projectContainer(decode(t, inspectWeb), decode(t, inspectImage), "demo")
	desired := normalizeDesired(KindService, parseYAML(desiredWeb), "/opt/composecd/demo")
	for k := range live {
		if _, ok := desired[k]; !ok {
			delete(live, k)
		}
	}
	delete(desired, "depends_on")
	ly, _ := encodeYAML(live)
	dy, _ := encodeYAML(desired)
	if ly != dy {
		t.Errorf("live:\n%s\ndesired:\n%s", ly, dy)
	}
	if strings.Contains(ly, "NGINX_VERSION") || strings.Contains(ly, "maintainer") || strings.Contains(ly, "0123456789") {
		t.Errorf("image defaults or anonymous volumes leaked:\n%s", ly)
	}

	// A hand-edited variable shows up as a difference.
	in := decode(t, inspectWeb)
	in["Config"].(map[string]interface{})["Env"] = []interface{}{"MODE=debug", "API_TOKEN=hunter2"}
	changed, _ := encodeYAML(projectContainer(in, decode(t, inspectImage), "demo"))
	if !strings.Contains(changed, "MODE: debug") {
		t.Errorf("changed env:\n%s", changed)
	}
}

func TestFoldInspect(t *testing.T) {
	in := decode(t, inspectWeb)
	redactEnv(in)
	ins, err := foldInspect("demo-web-1", KindContainer, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"GraphDriver", "LogPath", "SandboxKey", "com.docker.compose.project", "hunter2", "Privileged", "ExitCode"} {
		if strings.Contains(ins.YAML, gone) {
			t.Errorf("%s not folded:\n%s", gone, ins.YAML)
		}
	}
	for _, kept := range []string{"tier: front", "API_TOKEN=<redacted>", "unless-stopped", "Status: healthy"} {
		if !strings.Contains(ins.YAML, kept) {
			t.Errorf("%s missing:\n%s", kept, ins.YAML)
		}
	}
	paths := map[string]bool{}
	for _, f := range ins.Folded {
		paths[f.Path] = true
	}
	for _, p := range []string{"Engine metadata", "State.Health.Log", "Config.Labels (com.docker.compose.*)", "HostConfig (unset)"} {
		if !paths[p] {
			t.Errorf("no fold %q in %v", p, paths)
		}
	}
	if f := ins.Folded[0]; f.Path != "Engine metadata" || !strings.Contains(f.YAML, "GraphDriver:") || f.Fields != 4 {
		t.Errorf("metadata fold = %+v", f)
	}
	// The input is left alone.
	if _, ok := decode(t, inspectWeb)["GraphDriver"]; !ok {
		t.Fatal("fixture")
	}
	if _, ok := in["GraphDriver"]; !ok {
		t.Error("foldInspect changed its input")
	}
}

func TestNormalizeDesired(t *testing.T) {
	d := normalizeDesired(KindService, parseYAML(`command: echo "hello world"
healthcheck: {test: curl -f localhost, interval: 30s, retries: 3}
ports: ["127.0.0.1:53:53/udp", "9000"]
`), "/p")
	got, _ := encodeYAML(d)
	for _, want := range []string{"- echo\n", "- hello world", "- CMD-SHELL", "interval: 30s", "retries: \"3\"", "host_ip: 127.0.0.1", "protocol: udp", "target: \"9000\""} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}
