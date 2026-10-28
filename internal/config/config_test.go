package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDir(t *testing.T) {
	objs, err := LoadDir("testdata/root")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs.Hosts) != 2 || len(objs.Applications) != 2 {
		t.Fatalf("hosts %d, apps %d", len(objs.Hosts), len(objs.Applications))
	}
	pi := objs.Host("pi")
	if pi == nil || pi.Spec.ProjectRoot != "/srv/stacks" || pi.Spec.Env["TZ"] != "America/Chicago" || pi.Origin != "hosts.yaml:9" {
		t.Errorf("pi = %+v", pi)
	}
	media := objs.Application("media")
	if media == nil {
		t.Fatal("media missing")
	}
	s := media.Spec.Source
	if s.Path != "stacks/media" || s.ValueFiles[0] != "values-nas.yaml" || s.ValueFiles[1] != "../shared/values.yaml" || s.Env["PUID"] != "1000" || media.Origin != "apps/media.yaml:1" {
		t.Errorf("media source = %+v (origin %s)", s, media.Origin)
	}
	o := media.Spec.SyncPolicy.Options
	if media.Spec.SyncPolicy.Automated == nil || o.WaitOrDefault() || o.TimeoutOrDefault() != 90*time.Second ||
		o.HistoryMaxOrDefault() != 3 || !o.Atomic || o.Pull != "missing" {
		t.Errorf("media sync policy = %+v", media.Spec.SyncPolicy)
	}
	mon := objs.Application("monitoring")
	if !mon.Spec.Source.IsOCI() || mon.Spec.SyncPolicy.Automated != nil {
		t.Errorf("monitoring = %+v", mon.Spec)
	}
	if o := mon.Spec.SyncPolicy.Options; !o.WaitOrDefault() || o.TimeoutOrDefault() != DefaultTimeout || o.HistoryMaxOrDefault() != 10 {
		t.Errorf("defaults = %+v", o)
	}

	objs.Resolve(Root{RepoURL: "https://git.lan/homelab.git", TargetRevision: "main"})
	if s := objs.Application("media").Spec.Source; s.RepoURL != "https://git.lan/homelab.git" || s.TargetRevision != "main" {
		t.Errorf("resolved = %+v", s)
	}
	if s := objs.Application("monitoring").Spec.Source; s.RepoURL != "" {
		t.Errorf("oci source should not get a repoURL: %+v", s)
	}
}

func TestResolveOtherRepo(t *testing.T) {
	objs := &Objects{Applications: []*Application{{Spec: AppSpec{Source: AppSource{RepoURL: "https://other/x.git"}}}}}
	objs.Resolve(Root{RepoURL: "https://git.lan/homelab.git", TargetRevision: "main"})
	if got := objs.Applications[0].Spec.Source.TargetRevision; got != "HEAD" {
		t.Errorf("targetRevision = %q", got)
	}
}

func TestValidationErrors(t *testing.T) {
	host := "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {}\n---\n"
	cases := map[string]string{
		"unknown field":   "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {address: x}\n",
		"unknown kind":    "apiVersion: composecd/v1alpha1\nkind: SyncWindow\n",
		"bad apiVersion":  "apiVersion: composecd/v2\nkind: Host\n",
		"bad name":        "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: Bad_Name}\n",
		"duplicate host":  host + host,
		"bad url":         "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {url: http://x}\n",
		"url and context": "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {url: tcp://x, context: y}\n",
		"tls without tcp": "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {url: ssh://x, tlsCertPath: /c}\n",
		"relative root":   "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {projectRoot: data}\n",
		"bad storage":     "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\nspec: {storage: s3}\n",
		"missing host":    "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nope}}\n",
		"no host":         "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}}\n",
		"escaping path":   host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: ../x}, destination: {host: nas}}\n",
		"escaping vf":     host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x, valueFiles: [../../y.yaml]}, destination: {host: nas}}\n",
		"absolute vf":     host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x, valueFiles: [/etc/passwd]}, destination: {host: nas}}\n",
		"oci and path":    host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {chart: oci://r/c, path: x}, destination: {host: nas}}\n",
		"not oci":         host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {chart: https://r/c}, destination: {host: nas}}\n",
		"version on path": host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x, version: '1'}, destination: {host: nas}}\n",
		"bad pull":        host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nas}, syncPolicy: {options: {pull: sometimes}}}\n",
		"bad timeout":     host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nas}, syncPolicy: {options: {timeout: soon}}}\n",
		"bad label":       "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas, labels: {'a b': c}}\n",
	}
	for name, doc := range cases {
		if _, err := Parse("test.yaml", []byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.Contains(err.Error(), "test.yaml") {
			t.Errorf("%s: error should name the file: %v", name, err)
		}
	}
}

func TestBootstrap(t *testing.T) {
	b, err := ParseBootstrap([]byte(`
root: {repoURL: https://git.lan/me/homelab.git, path: composecd}
repositories:
  - {url: https://git.lan/, username: bot, passwordFile: /run/secrets/git}
  - {url: https://git.lan/me/, username: me, passwordFile: /run/secrets/me}
`))
	if err != nil {
		t.Fatal(err)
	}
	if b.Listen != ":8080" || b.DataDir != DefaultDataDir || b.RefreshInterval != Duration(DefaultRefreshInterval) ||
		b.Concurrency != 4 || b.Root.TargetRevision != "HEAD" || b.Root.Path != "composecd" {
		t.Errorf("defaults: %+v", b)
	}
	if c := b.Credentials("https://git.lan/me/homelab.git"); c == nil || c.Username != "me" {
		t.Errorf("longest prefix: %+v", c)
	}
	if c := b.Credentials("https://github.com/x"); c != nil {
		t.Errorf("no match: %+v", c)
	}
	for name, doc := range map[string]string{
		"no root":   "listen: :9000\n",
		"bad field": "root: {repoURL: x}\nlisten2: y\n",
		"fast":      "root: {repoURL: x}\nrefreshInterval: 1s\n",
		"both":      "root: {repoURL: x}\nrepositories: [{url: x, passwordFile: a, sshKeyFile: b}]\n",
		"escape":    "root: {repoURL: x, path: ../y}\n",
	} {
		if _, err := ParseBootstrap([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
