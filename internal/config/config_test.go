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
		"bad permission":  host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nas}, permissions: {env: ['a/b/c']}}\n",
		"bad glob":        host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nas}, permissions: {secrets: ['[a']}}\n",
		"unknown grant":   host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nas}, permissions: {volumes: ['*']}}\n",
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

func TestPermissionsAllow(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		app, svc string
		want     bool
	}{
		{nil, "a", "web", false},
		{[]string{"*"}, "a", "web", true},
		{[]string{"media-*"}, "media-tv", "db", true},
		{[]string{"media-*"}, "books", "db", false},
		{[]string{"vault/db"}, "vault", "db", true},
		{[]string{"vault/db"}, "vault", "web", false},
		{[]string{"*/postgres*"}, "x", "postgres-16", true},
	} {
		if got := Allows(c.patterns, c.app, c.svc); got != c.want {
			t.Errorf("Allows(%v, %s, %s) = %v", c.patterns, c.app, c.svc, got)
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

func TestUndecodableObjectsAreReported(t *testing.T) {
	host := "apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: nas}\n---\n"
	app := func(name string) string {
		return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec: {source: {path: x}, destination: {host: nas}}\n"
	}
	cases := map[string]string{
		// A missing --- merges two Applications into one document with duplicate keys.
		"missing separator": host + app("a") + app("b"),
		"duplicate field":   host + "apiVersion: composecd/v1alpha1\nkind: Application\nkind: Application\nmetadata: {name: a}\nspec: {source: {path: x}, destination: {host: nas}}\n",
		"wrong type":        host + "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: [not, a, mapping]\n",
	}
	for name, doc := range cases {
		_, err := Parse("apps.yaml", []byte(doc))
		if err == nil {
			t.Errorf("%s: the bad object was skipped without an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "apps.yaml:5") {
			t.Errorf("%s: error should point at the object: %v", name, err)
		}
	}
	_, err := Parse("apps.yaml", []byte(cases["missing separator"]))
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Errorf("missing separator: %v", err)
	}

	// Documents that aren't composecd objects are still ignored, broken or not.
	objs, err := Parse("mixed.yaml", []byte(host+"- a list\n- document\n---\njust a string\n---\nkind: ConfigMap\nkind: ConfigMap\n---\n"+app("a")))
	if err != nil || len(objs.Applications) != 1 {
		t.Errorf("foreign documents: %v, %+v", err, objs)
	}
}

func TestBootstrapOIDC(t *testing.T) {
	ok := "root: {repoURL: x}\nauth:\n  oidc: {issuer: https://auth.lan/app/, clientID: composecd, redirectURL: https://cd.lan/auth/callback, adminGroups: [admins]}\n"
	b, err := ParseBootstrap([]byte(ok))
	if err != nil || b.Auth.OIDC.ClientID != "composecd" {
		t.Fatalf("oidc = %+v, %v", b.Auth.OIDC, err)
	}
	for name, doc := range map[string]string{
		"http issuer":  "root: {repoURL: x}\nauth: {oidc: {issuer: http://auth.lan, clientID: c, redirectURL: https://cd.lan/cb, adminGroups: [a]}}\n",
		"no client":    "root: {repoURL: x}\nauth: {oidc: {issuer: https://auth.lan, redirectURL: https://cd.lan/cb, adminGroups: [a]}}\n",
		"no path":      "root: {repoURL: x}\nauth: {oidc: {issuer: https://auth.lan, clientID: c, redirectURL: https://cd.lan, adminGroups: [a]}}\n",
		"no admins":    "root: {repoURL: x}\nauth: {oidc: {issuer: https://auth.lan, clientID: c, redirectURL: https://cd.lan/cb}}\n",
		"and disabled": "root: {repoURL: x}\nauth: {disabled: true, oidc: {issuer: https://auth.lan, clientID: c, redirectURL: https://cd.lan/cb, adminGroups: [a]}}\n",
	} {
		if _, err := ParseBootstrap([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSOPSAgeKeyBootstrap(t *testing.T) {
	b, err := ParseBootstrap([]byte("root: {repoURL: /repo}\nsopsAgeKeyFile: /run/secrets/age-key\n"))
	if err != nil || b.SOPSAgeKeyFile != "/run/secrets/age-key" {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestInfisicalBootstrap(t *testing.T) {
	b, err := ParseBootstrap([]byte("root: {repoURL: x}\ninfisical: {clientID: id, clientSecretFile: /run/secrets/i}\n"))
	if err != nil || b.Infisical.SiteURL != DefaultInfisicalSiteURL {
		t.Fatalf("%+v %v", b.Infisical, err)
	}
	for name, doc := range map[string]string{
		"no creds":   "infisical: {}",
		"id only":    "infisical: {clientID: id}",
		"both kinds": "infisical: {clientID: id, clientSecretFile: f, tokenFile: t}",
		"plain http": "infisical: {tokenFile: t, siteURL: http://infisical.lan}",
	} {
		if _, err := ParseBootstrap([]byte("root: {repoURL: x}\n" + doc + "\n")); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
