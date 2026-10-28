//go:build integration

// Integration tests drive the controller against a real Docker engine. Run them with:
//
//	go test -tags integration -run Integration -v ./internal/controller
//
// They need docker with the compose plugin and pull busybox. Everything they
// create is removed, including their records in composetpl's release volume.
package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

const itChart = "apiVersion: v2\nname: box\nversion: 0.1.0\nappVersion: '1.37'\n"

// itTemplate runs one busybox service that prints an inline config, with a
// named volume and a pre-upgrade hook.
const itTemplate = `services:
  box:
    image: busybox:{{ .Chart.AppVersion }}
    init: true
    command: ["sh", "-c", "cat /etc/greeting; exec sleep 86400"]
    # Compose may not recreate a container when only a config's content
    # changes, so the message is in a label too.
    labels: {message: {{ .Values.message | quote }}}
    volumes: [data:/data]
    configs:
      - {source: greeting, target: /etc/greeting}
  migrate:
    image: busybox:{{ .Chart.AppVersion }}
    command: ["echo", "migrating to {{ .Values.message }}"]
    x-composetpl: {hook: pre-upgrade}
volumes:
  data: {}
configs:
  greeting:
    content: {{ .Values.message | quote }}
`

// itBroken can't start: its image doesn't exist and may not be pulled.
const itBroken = `services:
  box:
    image: composecd-it-missing:never
    pull_policy: never
`

// newDockerEnv returns an env whose host "local" is the default Docker engine,
// with release history in storage ("local" or "volume"), and the name of a
// fresh project for the test's application. The root starts with no apps.
func newDockerEnv(t *testing.T, storageKind string) (*env, string) {
	t.Helper()
	requireCompose(t)
	name := fmt.Sprintf("ccd-it-%d", rand.Intn(1e6))
	dir := t.TempDir()
	e := &env{t: t, repo: t.TempDir()}
	e.git("init", "--quiet", "--initial-branch", "main")
	e.commit(map[string]string{
		"composecd/hosts.yaml": fmt.Sprintf("apiVersion: composecd/v1alpha1\nkind: Host\nmetadata: {name: local}\n"+
			"spec: {storage: %s, projectRoot: %q}\n", storageKind, filepath.ToSlash(filepath.Join(dir, "projects"))),
		"composecd/apps.yaml":            "# none yet\n",
		"charts/box/Chart.yaml":          itChart,
		"charts/box/values.yaml":         "message: hello\n",
		"charts/box/templates/box.yaml":  itTemplate,
		"charts/broken/Chart.yaml":       strings.Replace(itChart, "name: box", "name: broken", 1),
		"charts/broken/templates/a.yaml": itBroken,
	})
	st, err := store.Open(filepath.Join(dir, "composecd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backend := &hosts.CLI{DataDir: dir}
	boot := &config.Bootstrap{
		Concurrency: 2, RefreshInterval: config.Duration(time.Hour),
		Root: config.Root{RepoURL: e.repo, TargetRevision: "main", Path: "composecd"},
	}
	e.c = New(Options{
		Boot: boot, Git: gitcache.New(filepath.Join(dir, "git"), nil), Hosts: hosts.NewRegistry(backend),
		Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if storageKind == config.StorageVolume {
		existed := exec.Command("docker", "volume", "inspect", storage.DefaultVolume).Run() == nil
		t.Cleanup(func() {
			v := storage.NewVolume(nil, "")
			if hist, err := v.History(name); err == nil {
				_ = v.Delete(hist...)
			}
			if !existed {
				_ = exec.Command("docker", "volume", "rm", "-f", storage.DefaultVolume).Run()
			}
		})
	}
	t.Cleanup(func() { removeProject(name) })
	// Runs first: stop the event watchers, so removing the containers doesn't
	// get them healed.
	t.Cleanup(func() {
		e.c.Close()
		e.c.Wait()
	})
	return e, name
}

// requireCompose skips the test without Docker, except in CI, where the
// integration job must not pass by skipping everything.
func requireCompose(t *testing.T) {
	t.Helper()
	skip := t.Skip
	if os.Getenv("CI") != "" {
		skip = t.Fatal
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		skip("docker compose is not available")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skip("docker engine is not reachable")
	}
}

// removeProject removes everything compose created for project, including
// one-off hook containers and named volumes.
func removeProject(project string) {
	filter := "label=com.docker.compose.project=" + project
	for _, kind := range []string{"container", "volume", "network"} {
		args := []string{kind, "ls", "-q", "--filter", filter}
		if kind == "container" {
			args = []string{"ps", "-aq", "--filter", filter}
		}
		out, _ := exec.Command("docker", args...).Output()
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			rm := append([]string{kind, "rm", "-f"}, ids...)
			_ = exec.Command("docker", rm...).Run()
		}
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// containers returns "name state" for each of project's containers.
func containers(t *testing.T, project string) string {
	t.Helper()
	return docker(t, "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.oneoff=False", "--format", "{{.Names}} {{.State}}")
}

// releasesDir is where storage: local keeps the host's releases.
func releasesDir(e *env) string {
	return filepath.Join(e.c.Hosts.Backend.(*hosts.CLI).DataDir, "hosts", "local", "releases")
}

func itApp(name, chart, automated string) string {
	return "apiVersion: composecd/v1alpha1\nkind: Application\nmetadata: {name: " + name + "}\nspec:\n" +
		"  source: {path: charts/" + chart + "}\n  destination: {host: local}\n" +
		"  syncPolicy: {automated: " + automated + ", options: {timeout: 2m}}\n---\n"
}

// composetpl runs the composetpl CLI, built from the version composecd uses.
func composetpl(t *testing.T, env []string, args ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "composetpl")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/trevorswanson/composetpl/cmd/composetpl").CombinedOutput(); err != nil {
		t.Fatalf("building composetpl: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("composetpl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestIntegrationInstall(t *testing.T) {
	for _, kind := range []string{config.StorageLocal, config.StorageVolume} {
		t.Run(kind, func(t *testing.T) {
			e, name := newDockerEnv(t, kind)
			e.apps(itApp(name, "box", "{}"))
			e.refresh()

			st := e.app(name)
			if st.Sync.Status != Synced || st.Health.Status != health.Healthy || st.Release.Revision != 1 || st.Error != "" {
				t.Fatalf("after install: sync %+v health %+v release %+v error %q", st.Sync, st.Health, st.Release, st.Error)
			}
			if got := containers(t, name); got != name+"-box-1 running" {
				t.Errorf("containers = %q", got)
			}
			// The inline config reached the container (the README recommends
			// configs.content for remote hosts, where file: paths don't exist).
			if got := docker(t, "exec", name+"-box-1", "cat", "/etc/greeting"); got != "hello" {
				t.Errorf("/etc/greeting = %q", got)
			}

			// composetpl sees the release composecd deployed.
			ctplEnv := []string{"COMPOSETPL_STORAGE=" + kind}
			if kind == config.StorageLocal {
				ctplEnv = append(ctplEnv, "COMPOSETPL_STATE_DIR="+releasesDir(e))
			}
			list := composetpl(t, ctplEnv, "list", "--selector", LabelManagedBy+"="+ManagedBy)
			if !strings.Contains(list, name) || !strings.Contains(list, "deployed") || !strings.Contains(list, "running(1)") {
				t.Errorf("composetpl list:\n%s", list)
			}
		})
	}
}

func TestIntegrationUpgradeRunsHooks(t *testing.T) {
	e, name := newDockerEnv(t, config.StorageLocal)
	e.apps(itApp(name, "box", "{}"))
	e.refresh()
	first := docker(t, "inspect", "--format", "{{.Id}}", name+"-box-1")

	e.commit(map[string]string{"charts/box/values.yaml": "message: bonjour\n"})
	e.refresh()
	st := e.app(name)
	if st.Sync.Status != Synced || st.Release.Revision != 2 || st.Operation == nil || st.Operation.Phase != store.PhaseSucceeded {
		t.Fatalf("after upgrade: sync %+v release %+v op %+v", st.Sync, st.Release, st.Operation)
	}
	// The status's operation has no log; read it from the store.
	op, err := e.c.Store.Operation(st.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(op.Log, "Running pre-upgrade hook migrate") {
		t.Errorf("the pre-upgrade hook didn't run:\n%s", op.Log)
	}
	if got := docker(t, "exec", name+"-box-1", "cat", "/etc/greeting"); got != "bonjour" {
		t.Errorf("/etc/greeting = %q", got)
	}
	if docker(t, "inspect", "--format", "{{.Id}}", name+"-box-1") == first {
		t.Error("the container was not recreated with the new config")
	}
	if got := containers(t, name); got != name+"-box-1 running" {
		t.Errorf("containers = %q", got)
	}
}

func TestIntegrationSelfHeal(t *testing.T) {
	// Set before the controller starts; nothing else reads them concurrently.
	base, debounce := healBackoffBase, eventDebounce
	healBackoffBase, eventDebounce = time.Second, 3*time.Second
	t.Cleanup(func() { healBackoffBase, eventDebounce = base, debounce })
	e, name := newDockerEnv(t, config.StorageLocal)
	e.apps(itApp(name, "box", "{selfHeal: true}"))
	e.refresh()
	container := name + "-box-1"
	// settle lets the reconcile that container events scheduled run.
	settle := func() {
		time.Sleep(eventDebounce + time.Second)
		e.c.Wait()
	}
	heals := func() int {
		ops, err := e.c.Store.Operations(name, 20)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, op := range ops {
			if op.Kind == store.KindHeal {
				n++
			}
		}
		return n
	}
	check := func(how string) {
		t.Helper()
		if op := e.app(name).Operation; op == nil || op.Kind != store.KindHeal || op.Phase != store.PhaseSucceeded || op.ReleaseRevision != 1 {
			t.Errorf("%s: operation = %+v", how, op)
		}
		if got := containers(t, name); got != container+" running" {
			t.Errorf("%s: containers = %q", how, got)
		}
		st := e.app(name)
		if st.Sync.Status != Synced || st.Release.Revision != 1 {
			t.Errorf("%s: sync %+v release %+v", how, st.Sync, st.Release)
		}
		if hist, err := e.c.History(name); err != nil || len(hist) != 1 {
			t.Errorf("%s: a heal made a new revision: %+v, %v", how, hist, err)
		}
	}

	// A refresh finds the container missing and recreates it. The event watcher
	// also hears the removal, but waits eventDebounce, so the refresh is first.
	settle()
	docker(t, "rm", "-f", container)
	e.refresh()
	if n := heals(); n != 1 {
		t.Fatalf("heals after refresh = %d", n)
	}
	check("refresh")

	// Let the reconcile scheduled by that removal and the heal backoff pass.
	settle()

	// With no refresh, the container events alone get the app healed.
	docker(t, "rm", "-f", container)
	waitFor(t, "the event-driven heal", 60*time.Second, func() bool {
		e.c.Wait()
		return heals() == 2
	})
	check("events")
}

func TestIntegrationPruneKeepsVolumes(t *testing.T) {
	e, name := newDockerEnv(t, config.StorageLocal)
	// The second, manual app keeps the root non-empty: an empty root is never pruned.
	keep := itApp(name+"-keep", "box", "null")
	e.apps(itApp(name, "box", "{prune: true}"), keep)
	e.refresh()
	if st := e.app(name); st.Sync.Status != Synced {
		t.Fatalf("not deployed: sync %+v error %q", st.Sync, st.Error)
	}
	volume := name + "_data"
	docker(t, "exec", name+"-box-1", "touch", "/data/kept")

	e.apps(keep)
	e.refresh()
	if got := containers(t, name); got != "" {
		t.Errorf("containers after prune = %q", got)
	}
	ops, _ := e.c.Store.Operations(name, 1)
	if len(ops) != 1 || ops[0].Kind != store.KindPrune || ops[0].Phase != store.PhaseSucceeded {
		t.Errorf("prune operation = %+v", ops)
	}
	hist, err := storage.NewLocal(releasesDir(e)).History(name)
	if err != nil || hist[len(hist)-1].Status != release.StatusUninstalled {
		t.Errorf("history = %+v, %v", hist, err)
	}
	// The named volume and its data survive.
	if got := docker(t, "run", "--rm", "-v", volume+":/data", "busybox:1.37", "ls", "/data"); got != "kept" {
		t.Errorf("volume %s holds %q", volume, got)
	}
	if got := containers(t, name+"-keep"); got != "" {
		t.Errorf("the manual app was deployed: %q", got)
	}
}

func TestIntegrationFailedSyncIsNotRetried(t *testing.T) {
	e, name := newDockerEnv(t, config.StorageLocal)
	e.apps(itApp(name, "broken", "{}"))
	e.refresh()
	st := e.app(name)
	if st.Operation == nil || st.Operation.Phase != store.PhaseFailed || !strings.Contains(st.Operation.Message, "composecd-it-missing") {
		t.Fatalf("operation = %+v", st.Operation)
	}
	if st.Sync.Status != OutOfSync || !strings.Contains(st.Blocked, "the last sync of this revision failed") {
		t.Errorf("sync %+v blocked %q", st.Sync, st.Blocked)
	}

	e.refresh()
	ops, err := e.c.Store.Operations(name, 10)
	if err != nil || len(ops) != 1 {
		t.Errorf("operations after a second refresh = %+v, %v", ops, err)
	}
}

// waitFor polls cond until it holds or timeout passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
