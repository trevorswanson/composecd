package gitcache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trevorswanson/composecd/internal/config"
)

// testRepo is a local repository to clone from.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	r := &testRepo{t: t, dir: t.TempDir()}
	r.run("init", "--quiet", "--initial-branch", "main")
	return r
}

func (r *testRepo) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1", "HOME="+r.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (path -> content) and commits them, returning the SHA.
func (r *testRepo) commit(files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		p := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.run("add", "--all")
	r.run("commit", "--quiet", "-m", "update")
	return r.run("rev-parse", "HEAD")
}

func TestFetchResolveCheckout(t *testing.T) {
	repo := newTestRepo(t)
	first := repo.commit(map[string]string{"stacks/media/Chart.yaml": "name: media\n"})
	repo.run("tag", "v1")
	ctx := context.Background()
	c := New(t.TempDir(), nil)

	if _, err := c.Resolve(ctx, repo.dir, "main"); err == nil {
		t.Error("resolve before fetch should fail")
	}
	if err := c.Fetch(ctx, repo.dir); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []string{"main", "HEAD", "", "v1", first, first[:8]} {
		if got, err := c.Resolve(ctx, repo.dir, rev); err != nil || got != first {
			t.Errorf("resolve %q = %q, %v", rev, got, err)
		}
	}
	if _, err := c.Resolve(ctx, repo.dir, "nope"); err == nil {
		t.Error("unknown revision should fail")
	}
	if _, err := c.Resolve(ctx, repo.dir, "--output=x"); err == nil {
		t.Error("option-like revision should fail")
	}

	second := repo.commit(map[string]string{"stacks/media/Chart.yaml": "name: media\nversion: 2\n"})
	if got, _ := c.Resolve(ctx, repo.dir, "main"); got != first {
		t.Errorf("resolve without fetching = %q, want the cached %q", got, first)
	}
	if err := c.Fetch(ctx, repo.dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Resolve(ctx, repo.dir, "main"); got != second {
		t.Errorf("after fetch = %q, want %q", got, second)
	}
	if got, _ := c.Resolve(ctx, repo.dir, "v1"); got != first {
		t.Errorf("tag moved: %q", got)
	}

	dir1, err := c.Checkout(ctx, repo.dir, first)
	if err != nil {
		t.Fatal(err)
	}
	dir2, err := c.Checkout(ctx, repo.dir, second)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir1, "stacks/media/Chart.yaml")); string(b) != "name: media\n" {
		t.Errorf("first checkout = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, "stacks/media/Chart.yaml")); !strings.Contains(string(b), "version: 2") {
		t.Errorf("second checkout = %q", b)
	}
	if again, err := c.Checkout(ctx, repo.dir, first); err != nil || again != dir1 {
		t.Errorf("second checkout of the same commit = %q, %v", again, err)
	}
	if _, err := c.Checkout(ctx, repo.dir, "main"); err == nil {
		t.Error("checkout of a branch name should fail")
	}

	if err := c.Prune(map[string]bool{CheckoutKey(repo.dir, second): true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Error("pruned checkout still exists")
	}
	if _, err := os.Stat(dir2); err != nil {
		t.Error("kept checkout was pruned")
	}
}

func TestCredentialsEnv(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(t.TempDir(), func(url string) *config.Repository {
		switch {
		case strings.HasPrefix(url, "https://"):
			return &config.Repository{Username: "bot", PasswordFile: pw}
		case strings.HasPrefix(url, "ssh://"):
			return &config.Repository{SSHKeyFile: "/keys/it's", KnownHostsFile: "/keys/known"}
		}
		return nil
	})
	env, err := c.env("https://git.lan/x.git")
	if err != nil {
		t.Fatal(err)
	}
	// base64("bot:s3cret")
	if !contains(env, "GIT_CONFIG_VALUE_0=Authorization: Basic Ym90OnMzY3JldA==") || !contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("https env = %v", env)
	}
	env, _ = c.env("ssh://git@git.lan/x.git")
	want := `GIT_SSH_COMMAND=ssh -o BatchMode=yes -i '/keys/it'\''s' -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile='/keys/known'`
	if !contains(env, want) {
		t.Errorf("ssh env = %v", env)
	}
	if got := redact("https://bot:pw@git.lan/x.git"); got != "https://***@git.lan/x.git" {
		t.Errorf("redact = %q", got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
