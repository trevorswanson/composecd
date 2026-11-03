package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

// ErrRejected is a push the remote refused because the branch moved on since
// the commit's parent: fetch, redo the change against the new tip and retry.
var ErrRejected = errors.New("the branch moved on since the change was made")

// Author is who a commit is by.
type Author struct {
	Name  string
	Email string
}

// Commit is a change to some files of one branch.
type Commit struct {
	// Branch is the branch pushed to.
	Branch string
	// Parent is the commit the change was made against, normally the
	// branch's tip when it was read. The push fails with ErrRejected if the
	// branch has moved on since.
	Parent string
	// Files are the new contents of the files changed, by path from the
	// repository's root, with forward slashes; nil removes the file.
	Files   map[string][]byte
	Message string
	Author  Author
	// SigningKeyFile, when set, signs the commit with this SSH private key
	// (gpg.format=ssh).
	SigningKeyFile string
}

// CommitFiles commits c to the cached copy of url without a work tree, and
// pushes it to the branch. It builds the new tree with a private index, so
// concurrent checkouts are unaffected, and never force-pushes: a branch that
// moved on since c.Parent fails with ErrRejected. It returns the new commit,
// or "" when the files already have those contents.
func (c *Cache) CommitFiles(ctx context.Context, url string, ci Commit) (string, error) {
	if !shaRegexp.MatchString(ci.Parent) {
		return "", fmt.Errorf("commit needs a full parent SHA, got %q", ci.Parent)
	}
	if ci.Branch == "" || strings.HasPrefix(ci.Branch, "-") || strings.ContainsAny(ci.Branch, " \t\n:") {
		return "", fmt.Errorf("invalid branch %q", ci.Branch)
	}
	defer c.lock(url)()
	repo := c.repoDir(url)
	idx, err := os.CreateTemp(c.Dir, "commit-*.index")
	if err != nil {
		return "", err
	}
	index := idx.Name()
	idx.Close()
	// git refuses an index file that exists but is empty.
	os.Remove(index)
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := c.gitEnv(ctx, url, repo, env, "read-tree", ci.Parent); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(ci.Files))
	for p := range ci.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if clean := path.Clean(p); clean != p || path.IsAbs(p) || p == "." || strings.HasPrefix(p, "../") {
			return "", fmt.Errorf("invalid path %q", p)
		}
		if ci.Files[p] == nil {
			// Mode 0 removes the entry; --force-remove would need a work tree.
			if _, err := c.gitIn(ctx, url, repo, env, []byte("0 "+strings.Repeat("0", 40)+"\t"+p+"\n"), "update-index", "--index-info"); err != nil {
				return "", err
			}
			continue
		}
		blob, err := c.gitIn(ctx, url, repo, nil, ci.Files[p], "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		// Keep the file's mode (an executable stays executable).
		mode := "100644"
		if out, err := c.gitEnv(ctx, url, repo, env, "ls-files", "--stage", "--", p); err == nil {
			if f := strings.Fields(out); len(f) > 0 {
				mode = f[0]
			}
		}
		if _, err := c.gitEnv(ctx, url, repo, env, "update-index", "--add", "--cacheinfo",
			mode+","+strings.TrimSpace(blob)+","+p); err != nil {
			return "", err
		}
	}
	tree, err := c.gitEnv(ctx, url, repo, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	if old, err := c.git(ctx, url, repo, "rev-parse", ci.Parent+"^{tree}"); err == nil && strings.TrimSpace(old) == tree {
		return "", nil
	}
	who := []string{"GIT_AUTHOR_NAME=" + ci.Author.Name, "GIT_AUTHOR_EMAIL=" + ci.Author.Email,
		"GIT_COMMITTER_NAME=" + ci.Author.Name, "GIT_COMMITTER_EMAIL=" + ci.Author.Email}
	args := []string{"commit-tree", tree, "-p", ci.Parent, "-F", "-"}
	if ci.SigningKeyFile != "" {
		args = append([]string{"-c", "gpg.format=ssh", "-c", "user.signingkey=" + ci.SigningKeyFile}, args...)
		args = append(args, "-S")
	}
	sha, err := c.gitIn(ctx, url, repo, who, []byte(ci.Message), args...)
	if err != nil {
		return "", err
	}
	sha = strings.TrimSpace(sha)
	if _, err := c.git(ctx, url, repo, "push", "--quiet", "origin", sha+":refs/heads/"+ci.Branch); err != nil {
		if rejected(err) {
			return "", fmt.Errorf("pushing to %s %s: %w", redact(url), ci.Branch, ErrRejected)
		}
		return "", err
	}
	// The mirror has the commit already; record where the branch is now, so
	// the next read sees it without waiting for a fetch.
	if _, err := c.git(ctx, url, repo, "update-ref", "refs/heads/"+ci.Branch, sha); err != nil {
		return sha, err
	}
	return sha, nil
}

// rejected is true for a push refused because the branch isn't an ancestor of
// the commit pushed.
func rejected(err error) bool {
	msg := err.Error()
	for _, s := range []string{"non-fast-forward", "fetch first", "[rejected]", "stale info"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Branch is the branch rev names in the cached copy of url: rev itself when
// it is one, or the branch the remote's HEAD points to when rev is "" or
// HEAD. It is an error when rev isn't a branch (a tag or a commit).
func (c *Cache) Branch(ctx context.Context, url, rev string) (string, error) {
	defer c.lock(url)()
	repo := c.repoDir(url)
	if rev == "" || rev == "HEAD" {
		out, err := c.git(ctx, url, repo, "symbolic-ref", "--quiet", "HEAD")
		if err != nil {
			return "", fmt.Errorf("finding the default branch of %s: %w", redact(url), err)
		}
		return strings.TrimPrefix(strings.TrimSpace(out), "refs/heads/"), nil
	}
	if strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("invalid revision %q", rev)
	}
	if _, err := c.git(ctx, url, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+rev); err != nil {
		return "", fmt.Errorf("%q is not a branch of %s", rev, redact(url))
	}
	return rev, nil
}

// TreeID is the ID of the tree or blob at path in commit sha of url, which
// changes exactly when something under path does.
func (c *Cache) TreeID(ctx context.Context, url, sha, p string) (string, error) {
	if !shaRegexp.MatchString(sha) {
		return "", fmt.Errorf("tree needs a full commit SHA, got %q", sha)
	}
	defer c.lock(url)()
	out, err := c.git(ctx, url, c.repoDir(url), "rev-parse", "--verify", "--quiet", sha+":"+strings.TrimPrefix(path.Clean("/"+p), "/"))
	if err != nil {
		return "", fmt.Errorf("%s has no %s", short(sha), p)
	}
	return strings.TrimSpace(out), nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
