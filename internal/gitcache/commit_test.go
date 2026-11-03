package gitcache

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pushable is a test repository that accepts pushes to its checked-out branch.
func pushable(t *testing.T) *testRepo {
	t.Helper()
	r := newTestRepo(t)
	r.run("config", "receive.denyCurrentBranch", "updateInstead")
	return r
}

func TestCommitFiles(t *testing.T) {
	repo := pushable(t)
	repo.commit(map[string]string{"charts/web/values.yaml": "tag: 1.27.1\n", "run.sh": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(repo.dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.run("add", "run.sh")
	repo.run("commit", "--quiet", "-m", "exec")
	ctx := context.Background()
	c := New(t.TempDir(), nil)
	if err := c.Fetch(ctx, repo.dir); err != nil {
		t.Fatal(err)
	}
	parent, err := c.Resolve(ctx, repo.dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ci := Commit{Branch: "main", Parent: parent, Message: "composecd: update images\n\nweb: 1.27.3\n",
		Author: Author{Name: "composecd", Email: "composecd@localhost"},
		Files:  map[string][]byte{"charts/web/values.yaml": []byte("tag: 1.27.3\n"), "run.sh": []byte("#!/bin/sh\nexit 0\n")}}
	sha, err := c.CommitFiles(ctx, repo.dir, ci)
	if err != nil || sha == "" {
		t.Fatalf("commit = %q, %v", sha, err)
	}
	if got := repo.run("rev-parse", "main"); got != sha {
		t.Errorf("remote main = %s, want %s", got, sha)
	}
	if got := repo.run("show", "main:charts/web/values.yaml"); got != "tag: 1.27.3" {
		t.Errorf("values = %q", got)
	}
	if got := repo.run("log", "-1", "--format=%an <%ae>|%s|%P", "main"); got != "composecd <composecd@localhost>|composecd: update images|"+parent {
		t.Errorf("commit = %q", got)
	}
	if got := repo.run("ls-tree", "main", "run.sh"); !strings.HasPrefix(got, "100755") {
		t.Errorf("mode lost: %s", got)
	}
	// The mirror moved with the push.
	if got, _ := c.Resolve(ctx, repo.dir, "main"); got != sha {
		t.Errorf("mirror main = %s", got)
	}

	// Writing what is already there commits nothing.
	ci.Parent = sha
	if again, err := c.CommitFiles(ctx, repo.dir, ci); err != nil || again != "" {
		t.Errorf("no-op commit = %q, %v", again, err)
	}

	// A branch that moved on is never overwritten.
	theirs := repo.commit(map[string]string{"README": "hi\n"})
	ci.Files = map[string][]byte{"charts/web/values.yaml": []byte("tag: 1.27.4\n")}
	if _, err := c.CommitFiles(ctx, repo.dir, ci); !errors.Is(err, ErrRejected) {
		t.Fatalf("stale push: %v", err)
	}
	if got := repo.run("rev-parse", "main"); got != theirs {
		t.Errorf("remote main = %s, want theirs %s", got, theirs)
	}

	if _, err := c.CommitFiles(ctx, repo.dir, Commit{Branch: "main", Parent: sha, Files: map[string][]byte{"../x": nil}}); err == nil {
		t.Error("a path outside the repository was accepted")
	}
	if _, err := c.CommitFiles(ctx, repo.dir, Commit{Branch: "-f", Parent: sha}); err == nil {
		t.Error("an option-like branch was accepted")
	}
}

func TestBranchAndTreeID(t *testing.T) {
	repo := newTestRepo(t)
	sha := repo.commit(map[string]string{"charts/web/values.yaml": "tag: 1\n"})
	repo.run("tag", "v1")
	ctx := context.Background()
	c := New(t.TempDir(), nil)
	if err := c.Fetch(ctx, repo.dir); err != nil {
		t.Fatal(err)
	}
	for rev, want := range map[string]string{"": "main", "HEAD": "main", "main": "main"} {
		if got, err := c.Branch(ctx, repo.dir, rev); err != nil || got != want {
			t.Errorf("branch %q = %q, %v", rev, got, err)
		}
	}
	for _, rev := range []string{"v1", sha, "nope"} {
		if _, err := c.Branch(ctx, repo.dir, rev); err == nil {
			t.Errorf("%q is not a branch", rev)
		}
	}
	tree, err := c.TreeID(ctx, repo.dir, sha, "charts/web")
	if err != nil || tree != repo.run("rev-parse", sha+":charts/web") {
		t.Errorf("tree = %q, %v", tree, err)
	}
	if _, err := c.TreeID(ctx, repo.dir, sha, "charts/nope"); err == nil {
		t.Error("missing path has a tree")
	}
}

func TestCommitFilesSigned(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	repo := pushable(t)
	repo.commit(map[string]string{"values.yaml": "tag: 1\n"})
	keys := t.TempDir()
	key := filepath.Join(keys, "signing")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "composecd", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	signers := filepath.Join(keys, "allowed_signers")
	if err := os.WriteFile(signers, []byte("composecd@localhost "+string(pub)), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	c := New(t.TempDir(), nil)
	if err := c.Fetch(ctx, repo.dir); err != nil {
		t.Fatal(err)
	}
	parent, _ := c.Resolve(ctx, repo.dir, "main")
	sha, err := c.CommitFiles(ctx, repo.dir, Commit{Branch: "main", Parent: parent, Message: "signed\n",
		Author: Author{Name: "composecd", Email: "composecd@localhost"}, SigningKeyFile: key,
		Files: map[string][]byte{"values.yaml": []byte("tag: 2\n")}})
	if err != nil {
		t.Fatal(err)
	}
	repo.run("config", "gpg.format", "ssh")
	repo.run("config", "gpg.ssh.allowedSignersFile", signers)
	if out := repo.run("verify-commit", "--raw", sha); out != "" && !strings.Contains(out, "Good") {
		t.Errorf("verify-commit: %s", out)
	}
	if !strings.Contains(repo.run("cat-file", "commit", sha), "-----BEGIN SSH SIGNATURE-----") {
		t.Error("the commit isn't signed")
	}
}
