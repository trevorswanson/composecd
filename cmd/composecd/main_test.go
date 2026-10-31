package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/compose/composetest"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/api"
	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/events"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

func TestMain(m *testing.M) {
	// Never read or write the developer's own saved login.
	dir, err := os.MkdirTemp("", "composecd-cli-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("COMPOSECD_LOGIN_FILE", filepath.Join(dir, "login.yaml"))
	os.Unsetenv("COMPOSECD_SERVER")
	os.Unsetenv("COMPOSECD_TOKEN")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fakeBackend struct {
	dir  string
	fake *composetest.Fake
}

func (b *fakeBackend) Runner(*hosts.Host, []string, io.Writer) compose.Runner { return b.fake }

func (b *fakeBackend) Watch(ctx context.Context, _ *hosts.Host, _ events.Handlers) error {
	<-ctx.Done()
	return ctx.Err()
}

func (b *fakeBackend) Store(h *hosts.Host) storage.Driver {
	return storage.NewLocal(filepath.Join(b.dir, h.Name()))
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// startServer runs the controller and API on a repository built from the
// example under examples/gitops, with a fake compose runner.
func startServer(t *testing.T) (*httptest.Server, *controller.Controller, *composetest.Fake) {
	t.Helper()
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS("../../examples/gitops")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "--quiet", "--initial-branch", "main")
	git(t, repo, "add", "--all")
	git(t, repo, "commit", "--quiet", "-m", "init")

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := composetest.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := controller.New(controller.Options{
		Boot: &config.Bootstrap{Concurrency: 2, RefreshInterval: config.Duration(config.DefaultRefreshInterval),
			Root: config.Root{RepoURL: repo, TargetRevision: "main", Path: "composecd"}},
		Git:   gitcache.New(filepath.Join(dir, "git"), nil),
		Hosts: hosts.NewRegistry(&fakeBackend{dir: filepath.Join(dir, "releases"), fake: fake}),
		Store: st, Log: log,
	})
	c.RefreshAll(context.Background())
	authn := &auth.Authenticator{Store: st, AdminToken: "test-token-0123456789"}
	srv := httptest.NewServer(api.New(c, api.Options{Auth: authn, Version: "test", Log: log}))
	// Cleanups run last-registered first: stop serving, then let background
	// operations (app refresh, resume) finish before the temp dirs are removed.
	// On Windows, files a running git still holds can't be deleted.
	t.Cleanup(func() {
		c.Close()
		c.Wait()
	})
	t.Cleanup(srv.Close)
	return srv, c, fake
}

func cli(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--server", srv.URL, "--token", "test-token-0123456789"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func mustCLI(t *testing.T, srv *httptest.Server, args ...string) string {
	t.Helper()
	out, err := cli(t, srv, args...)
	if err != nil {
		t.Fatalf("composecd %v: %v\n%s", args, err, out)
	}
	return out
}

func expect(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestCLIAgainstServer(t *testing.T) {
	srv, _, fake := startServer(t)

	out := mustCLI(t, srv, "app", "list")
	expect(t, out, "NAME", "whoami", "nas", "Synced", "Healthy", "yes", "dashboard", "OutOfSync", "not deployed")

	out = mustCLI(t, srv, "app", "get", "whoami")
	expect(t, out, "Name:       whoami", "Sync:       Synced, automated", "Health:     Healthy",
		"Release:    revision 1, deployed, whoami-0.1.0", "CONTAINERS:", "whoami-web-1")

	out, err := cli(t, srv, "app", "diff", "dashboard", "--exit-code")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Errorf("diff --exit-code of an undeployed app: %v", err)
	}
	expect(t, out, "dashboard is not deployed yet", "dashboard, services/dozzle, added:")

	out = mustCLI(t, srv, "app", "sync", "dashboard", "--logs")
	expect(t, out, "Operation #2 Succeeded: composecd sync to")
	if _, ok := fake.Running["dashboard"]; !ok {
		t.Error("dashboard was not deployed")
	}
	mustCLI(t, srv, "app", "diff", "dashboard", "--exit-code")

	expect(t, mustCLI(t, srv, "app", "history", "whoami"), "REVISION", "1 ", "deployed", "composecd sync to")
	expect(t, mustCLI(t, srv, "app", "operations", "dashboard"), "sync", "admin", "Succeeded")
	expect(t, mustCLI(t, srv, "operation", "list"), "APP", "whoami", "auto", "dashboard", "admin")
	expect(t, mustCLI(t, srv, "operation", "get", "1", "-o", "json"), `"app": "whoami"`, `"phase": "Succeeded"`)
	expect(t, mustCLI(t, srv, "host", "list"), "nas", "Up", "29.0.0", "2.30.0")

	expect(t, mustCLI(t, srv, "app", "pause", "whoami", "--reason", "testing"), "paused")
	expect(t, mustCLI(t, srv, "app", "list"), "paused")
	expect(t, mustCLI(t, srv, "app", "resume", "whoami"), "resumed")
	expect(t, mustCLI(t, srv, "app", "list", "-o", "yaml"), "name: whoami", "status: Synced")
	expect(t, mustCLI(t, srv, "window", "list"), "movie-night", "deny", "0 19 * * 5,6 America/Chicago", "allowed")
	expect(t, mustCLI(t, srv, "host", "maintenance", "nas", "on", "--reason", "disks"), "Maintenance mode of nas is on")
	expect(t, mustCLI(t, srv, "host", "list"), "maintenance: disks")
	expect(t, mustCLI(t, srv, "host", "maintenance", "nas", "off"), "is off")
	if _, err := cli(t, srv, "host", "maintenance", "nas", "maybe"); err == nil {
		t.Error("maintenance accepted a bad state")
	}
	expect(t, mustCLI(t, srv, "app", "get", "dashboard"), "Depends on: whoami")
	expect(t, mustCLI(t, srv, "refresh"), "Refresh requested")
	expect(t, mustCLI(t, srv, "app", "refresh", "whoami"), "Refresh of whoami requested")
	expect(t, mustCLI(t, srv, "app", "check-images", "whoami"), "Image check of whoami requested")
	expect(t, mustCLI(t, srv, "app", "check-images", "--all"), "Image check of every application requested")
	for _, args := range [][]string{{"app", "check-images"}, {"app", "check-images", "whoami", "--all"}, {"app", "check-images", "nope"}} {
		if _, err := cli(t, srv, args...); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}

	if _, err := cli(t, srv, "app", "get", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown app: %v", err)
	}
	cmd := newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "wrong", "app", "list"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "COMPOSECD_TOKEN") {
		t.Errorf("bad token: %v", err)
	}
}

func TestPrintImages(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, &controller.AppStatus{Images: []controller.ImageStatus{
		{Service: "web", Image: "nginx:1", Pin: controller.PinComposecd, Deployed: "sha256:aaaaaaaaaaaaaaaaaaaa", Latest: "sha256:bbbbbbbbbbbbbbbbbbbb",
			UpdateAvailable: true, Applicable: true},
		{Service: "db", Image: "postgres:16@sha256:cc", Pin: controller.PinGit, Latest: "sha256:dddddddddddddddddddd", UpdateAvailable: true,
			Note: "the digest is pinned in Git; change it there to update"},
		{Service: "app", Image: "app:${TAG}", Note: "the image uses a variable"},
	}})
	expect(t, out.String(), "IMAGES:", "sha256:aaaaaaaaaaaa", "sha256:bbbbbbbbbbbb", "available: sync to apply",
		"available, not applied: the digest is pinned in Git", "the image uses a variable")
}

func TestValidateExample(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"validate", "../../examples/gitops/composecd"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	expect(t, out.String(), "1 host, 2 applications and 1 sync window are valid")

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "x.yaml"), []byte("apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: a}\nspec: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"validate", bad})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "destination.host is required") {
		t.Errorf("invalid dir: %v", err)
	}
}

func TestAdminToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	boot := &config.Bootstrap{DataDir: t.TempDir()}
	first, err := adminToken(boot, log)
	if err != nil || len(first) != 48 {
		t.Fatalf("generated %q, %v", first, err)
	}
	if again, _ := adminToken(boot, log); again != first {
		t.Error("the generated token changed on restart")
	}
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boot.Auth.TokenFile = f
	if _, err := adminToken(boot, log); err == nil {
		t.Error("a short token should be rejected")
	}
	if err := os.WriteFile(f, []byte("a-long-enough-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := adminToken(boot, log); err != nil || got != "a-long-enough-token" {
		t.Errorf("token file: %q, %v", got, err)
	}
	boot.Auth.Disabled = true
	if got, _ := adminToken(boot, log); got != "" {
		t.Errorf("disabled: %q", got)
	}
}

// cliIn runs the CLI with stdin, and only the flags given.
func cliIn(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestAccountsCLI(t *testing.T) {
	srv, _, _ := startServer(t)
	admin := []string{"--server", srv.URL, "--token", "test-token-0123456789"}
	run := func(stdin string, args ...string) string {
		t.Helper()
		out, err := cliIn(t, stdin, append(append([]string{}, admin...), args...)...)
		if err != nil {
			t.Fatalf("composecd %v: %v\n%s", args, err, out)
		}
		return out
	}

	tok := strings.TrimSpace(run("", "token", "create", "grafana"))
	if !strings.HasPrefix(tok, "ccd_") {
		t.Fatalf("token = %q", tok)
	}
	expect(t, run("", "token", "list"), "NAME", "grafana", "readonly", "admin")
	expect(t, run("long enough password\n", "user", "add", "kid", "--role", "readonly"), "User kid (readonly) added")
	expect(t, run("", "user", "list"), "kid", "readonly")
	expect(t, run("another long password\n", "user", "passwd", "kid"), "Password of kid changed")
	if out, err := cliIn(t, "short\n", append(append([]string{}, admin...), "user", "add", "x")...); err == nil || !strings.Contains(err.Error(), "at least 10") {
		t.Errorf("short password: %v %s", err, out)
	}

	// login checks and saves the token; later commands need no flags.
	out, err := cliIn(t, tok+"\n", "--server", srv.URL, "login")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	expect(t, out, "Signed in to "+srv.URL+" as grafana (readonly)")
	out, err = cliIn(t, "", "whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	expect(t, out, "grafana (readonly, via token) on "+srv.URL)
	if _, err := cliIn(t, "", "app", "list"); err != nil {
		t.Errorf("app list with the saved login: %v", err)
	}
	if _, err := cliIn(t, "", "app", "sync", "whoami"); err == nil || !strings.Contains(err.Error(), "admin role") {
		t.Errorf("readonly sync: %v", err)
	}
	if _, err := cliIn(t, "wrong-token\n", "--server", srv.URL, "login"); err == nil {
		t.Error("login with a bad token succeeded")
	}
	expect(t, run("", "token", "revoke", "grafana"), "Token grafana revoked")
	if _, err := cliIn(t, "", "whoami"); err == nil {
		t.Error("revoked saved token still works")
	}
	if out, _ := cliIn(t, "", "logout"); !strings.Contains(out, "Signed out") {
		t.Errorf("logout: %s", out)
	}
	expect(t, run("", "user", "delete", "kid"), "User kid deleted")

	// Operations record who ran them.
	run("", "app", "sync", "dashboard")
	expect(t, run("", "app", "operations", "dashboard"), "admin")
}

func (b *fakeBackend) Logs(context.Context, *hosts.Host, string, int, bool, io.Writer) error {
	return nil
}

func TestValidateRejectsPlainSOPSFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "values.sops.yaml"), []byte("password: accidentally-plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := cliIn(t, "", "validate", dir); err == nil || !strings.Contains(err.Error(), "no SOPS metadata") {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.sops.yaml"), []byte("password: ENC[data]\nsops: {age: []}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := cliIn(t, "", "validate", dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestOCIValuesRevisionsPrinted(t *testing.T) {
	a := controller.AppStatus{Source: config.AppSource{Chart: "oci://registry.lan/charts/app", ValuesFrom: &config.GitSource{RepoURL: "https://git.lan/values.git", TargetRevision: "prod"}}, Sync: controller.SyncStatus{ChartRef: "oci://registry.lan/charts/app:1.0.0@sha256:abc", ValuesRevision: "abcdef0123456789", DeployedValuesRevision: "123456789abcdef"}}
	var out bytes.Buffer
	printApp(&out, &a)
	for _, want := range []string{a.Sync.ChartRef, "Values:", "https://git.lan/values.git @ prod", "Values rev:", "abcdef0", "1234567"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s: %s", want, &out)
		}
	}
	if !strings.Contains(revision(a), "values abcdef0") {
		t.Fatal(revision(a))
	}
}

func TestApplicationSetCLI(t *testing.T) {
	srv, c, _ := startServer(t)
	if got := mustCLI(t, srv, "appset", "list", "-o", "json"); strings.TrimSpace(got) != "[]" {
		t.Fatal(got)
	}
	root := c.Boot.Root.RepoURL
	set := `apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: examples}
spec:
  generators:
    - gitDirectories: {directories: ['charts/whoami']}
  template:
    metadata: {name: 'generated-{{ .path.basename }}'}
    spec:
      source: {path: '{{ .path.path }}'}
      destination: {host: nas}
`
	if err := os.WriteFile(filepath.Join(root, "composecd", "sets.yaml"), []byte(set), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "--quiet", "-m", "add set")
	c.RefreshAll(context.Background())
	c.Wait()
	expect(t, mustCLI(t, srv, "appset", "list"), "examples", "generated-whoami", "true")
	expect(t, mustCLI(t, srv, "appset", "list", "-o", "json"), `"generated": 1`, `"ready": true`)
	out, err := cliIn(t, "", "validate", filepath.Join(root, "composecd"))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, out, "1 ApplicationSets will be expanded and fully validated by the server")
}
