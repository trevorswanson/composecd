package hosts

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/trevorswanson/composetpl/pkg/compose"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/config"
)

func host(name string, spec config.HostSpec) *config.Host {
	return &config.Host{Metadata: config.Metadata{Name: name}, Spec: spec}
}

func TestConnectionSettings(t *testing.T) {
	r := NewRegistry(&CLI{DataDir: t.TempDir()})
	r.Sync([]*config.Host{
		host("tcp", config.HostSpec{URL: "tcp://gpu:2376", TLSCertPath: "/certs", Env: map[string]string{"B": "2", "A": "1"}}),
		host("ctx", config.HostSpec{Context: "pi", ProjectRoot: "/srv/stacks", Storage: config.StorageLocal}),
		host("default", config.HostSpec{}),
		host("win", config.HostSpec{ProjectRoot: `C:\composecd`}),
	})
	tcp, ctx, def := r.Get("tcp"), r.Get("ctx"), r.Get("default")
	if got := tcp.Global(); !reflect.DeepEqual(got, []string{"--host", "tcp://gpu:2376"}) {
		t.Errorf("tcp global = %v", got)
	}
	if got := tcp.Env(); !reflect.DeepEqual(got, []string{"DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=/certs", "A=1", "B=2"}) {
		t.Errorf("tcp env = %v", got)
	}
	if got := ctx.Global(); !reflect.DeepEqual(got, []string{"--context", "pi"}) || def.Global() != nil {
		t.Errorf("globals = %v %v", got, def.Global())
	}
	if ctx.ProjectDir("media") != "/srv/stacks/media" || def.ProjectDir("media") != "/opt/composecd/media" ||
		r.Get("win").ProjectDir("media") != `C:\composecd\media` {
		t.Error("project dirs", ctx.ProjectDir("media"), def.ProjectDir("media"), r.Get("win").ProjectDir("media"))
	}
	if ctx.Store().Name() != "local" || def.Store().Name() != "volume" {
		t.Errorf("stores = %s %s", ctx.Store().Name(), def.Store().Name())
	}
	cli, ok := tcp.Configuration([]string{"X=1"}, io.Discard).Compose.(*compose.CLI)
	if !ok || !reflect.DeepEqual(cli.Global, tcp.Global()) || cli.Env[len(cli.Env)-1] != "X=1" {
		t.Errorf("configuration runner = %+v", cli)
	}
	if names := []string{r.All()[0].Name(), r.All()[3].Name()}; names[0] != "ctx" || names[1] != "win" {
		t.Errorf("All not sorted: %v", names)
	}
}

func TestSyncKeepsUnchangedHosts(t *testing.T) {
	r := NewRegistry(&CLI{})
	r.Sync([]*config.Host{host("a", config.HostSpec{URL: "ssh://a"}), host("b", config.HostSpec{})})
	a, b := r.Get("a"), r.Get("b")
	r.Sync([]*config.Host{host("a", config.HostSpec{URL: "ssh://a"}), host("b", config.HostSpec{URL: "ssh://b"})})
	if r.Get("a") != a {
		t.Error("an unchanged host was recreated")
	}
	if r.Get("b") == b {
		t.Error("a changed host was kept")
	}
	r.Sync(nil)
	if r.Get("a") != nil || len(r.All()) != 0 {
		t.Error("removed hosts are still there")
	}
}

type pingBackend struct {
	fail  error
	dir   string
	built int
}

type pingRunner struct {
	compose.Runner
	fail error
}

func (p pingRunner) Version(context.Context) (string, error)       { return "2.30.0", p.fail }
func (p pingRunner) ServerVersion(context.Context) (string, error) { return "29.0.0", nil }

func (b *pingBackend) Runner(*Host, []string, io.Writer) compose.Runner {
	return pingRunner{fail: b.fail}
}

func (b *pingBackend) Watch(context.Context, *Host, func(string)) error { return nil }

func (b *pingBackend) Store(h *Host) storage.Driver {
	b.built++
	return storage.NewLocal(filepath.Join(b.dir, h.Name()))
}

func TestPingAndInvalidate(t *testing.T) {
	b := &pingBackend{dir: t.TempDir()}
	r := NewRegistry(b)
	r.Sync([]*config.Host{host("nas", config.HostSpec{URL: "ssh://nas"})})
	h := r.Get("nas")
	if s := h.Status(); s.State != StateUnknown || s.URL != "ssh://nas" {
		t.Errorf("before ping: %+v", s)
	}
	if s := h.Ping(context.Background()); s.State != StateUp || s.DockerVersion != "29.0.0" || s.ComposeVersion != "2.30.0" {
		t.Errorf("up: %+v", s)
	}
	b.fail = errors.New("ssh: connect to host nas: connection refused")
	if s := h.Ping(context.Background()); s.State != StateDown || !strings.Contains(s.Error, "refused") {
		t.Errorf("down: %+v", s)
	}

	h.Store()
	h.Store()
	if b.built != 1 {
		t.Errorf("store built %d times", b.built)
	}
	release := h.Use()
	if h.Invalidate() {
		t.Error("invalidated while an operation was using the host")
	}
	release()
	if !h.Invalidate() {
		t.Error("idle host not invalidated")
	}
	h.Store()
	if b.built != 2 {
		t.Errorf("store not rebuilt after invalidate: %d", b.built)
	}
}

func (b *pingBackend) Logs(context.Context, *Host, string, int, bool, io.Writer) error { return nil }

func TestCLILogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake docker")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$@"
printf 'env:%s\n' "$LOG_TEST"
printf 'stderr\n' >&2
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := NewRegistry(&CLI{})
	r.Sync([]*config.Host{host("test", config.HostSpec{Context: "remote", Env: map[string]string{"LOG_TEST": "present"}})})
	var out strings.Builder
	err := r.Backend.Logs(context.Background(), r.Get("test"), "web-1", 200, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--context\nremote\nlogs\n--tail\n200\n--follow\n--\nweb-1", "env:present", "stderr"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s: %s", want, &out)
		}
	}
	for _, name := range []string{"", "--help"} {
		if err := r.Backend.Logs(context.Background(), r.Get("test"), name, 200, false, io.Discard); err == nil {
			t.Fatal("accepted bad name")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Backend.Logs(ctx, r.Get("test"), "web-1", 200, false, io.Discard); err == nil {
		t.Fatal("ignored cancellation")
	}
}
