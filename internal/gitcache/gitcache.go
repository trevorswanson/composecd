// Package gitcache keeps bare mirrors of Git repositories and read-only
// checkouts of the commits composecd renders, driving the git CLI.
//
// Layout under Dir:
//
//	repos/<key>.git          bare mirror of one repository URL
//	checkouts/<key>/<sha>/   the tree of one commit
package gitcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/trevorswanson/composecd/internal/config"
)

// Cache mirrors repositories under Dir.
type Cache struct {
	Dir string
	// Credentials returns the credentials for a repository URL, or nil.
	Credentials func(url string) *config.Repository
	// Timeout bounds each git command (default 2m).
	Timeout time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns a Cache rooted at dir.
func New(dir string, creds func(url string) *config.Repository) *Cache {
	return &Cache{Dir: dir, Credentials: creds}
}

var shaRegexp = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// key names a repository's directories.
func key(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:8])
}

func (c *Cache) lock(url string) func() {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	l, ok := c.locks[url]
	if !ok {
		l = &sync.Mutex{}
		c.locks[url] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (c *Cache) repoDir(url string) string {
	return filepath.Join(c.Dir, "repos", key(url)+".git")
}

// Fetch clones url into the cache, or fetches every branch and tag if it is
// already there.
func (c *Cache) Fetch(ctx context.Context, url string) error {
	defer c.lock(url)()
	dir := c.repoDir(url)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		tmp := dir + ".tmp"
		_ = os.RemoveAll(tmp)
		if _, err := c.git(ctx, url, "", "clone", "--bare", "--quiet", "--", url, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		return os.Rename(tmp, dir)
	}
	_, err := c.git(ctx, url, dir, "fetch", "--quiet", "--prune", "--prune-tags", "--force", "origin",
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	return err
}

// Resolve returns the commit a branch, tag, commit SHA or HEAD points to in the
// cached copy of url. It does not fetch.
func (c *Cache) Resolve(ctx context.Context, url, rev string) (string, error) {
	defer c.lock(url)()
	dir := c.repoDir(url)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("repository %s has not been fetched", url)
	}
	if rev == "" {
		rev = "HEAD"
	}
	if strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("invalid revision %q", rev)
	}
	// Prefer branches, then tags, then anything rev-parse understands (a SHA).
	for _, ref := range []string{"refs/heads/" + rev, "refs/tags/" + rev, rev} {
		if out, err := c.git(ctx, url, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return strings.TrimSpace(out), nil
		}
	}
	return "", fmt.Errorf("revision %q not found in %s", rev, url)
}

// Checkout returns a directory holding the tree of commit sha of url, creating
// it if needed. Callers must not modify it, except for vendoring chart
// dependencies.
func (c *Cache) Checkout(ctx context.Context, url, sha string) (string, error) {
	if !shaRegexp.MatchString(sha) {
		return "", fmt.Errorf("checkout needs a full commit SHA, got %q", sha)
	}
	defer c.lock(url)()
	dir := filepath.Join(c.Dir, "checkouts", key(url), sha)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	// A private index keeps concurrent checkouts from sharing the mirror's.
	index := tmp + ".index"
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	repo := c.repoDir(url)
	for _, args := range [][]string{
		{"--work-tree", tmp, "read-tree", sha},
		{"--work-tree", tmp, "checkout-index", "--all", "--force"},
	} {
		if _, err := c.gitEnv(ctx, url, repo, env, args...); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return dir, nil
}

// Prune deletes checkouts other than keep, whose keys are CheckoutKey(url, sha).
func (c *Cache) Prune(keep map[string]bool) error {
	root := filepath.Join(c.Dir, "checkouts")
	repos, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for k := range keep {
		wanted[k] = true
	}
	var errs []error
	for _, r := range repos {
		shas, err := os.ReadDir(filepath.Join(root, r.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, s := range shas {
			if !wanted[r.Name()+"/"+s.Name()] {
				errs = append(errs, os.RemoveAll(filepath.Join(root, r.Name(), s.Name())))
			}
		}
	}
	return errors.Join(errs...)
}

// CheckoutKey identifies a checkout for Prune.
func CheckoutKey(url, sha string) string { return key(url) + "/" + sha }

func (c *Cache) git(ctx context.Context, url, gitDir string, args ...string) (string, error) {
	return c.gitEnv(ctx, url, gitDir, nil, args...)
}

// gitEnv runs git against gitDir (if set) with url's credentials.
func (c *Cache) gitEnv(ctx context.Context, url, gitDir string, extra []string, args ...string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if gitDir != "" {
		args = append([]string{"--git-dir", gitDir}, args...)
	}
	env, err := c.env(url)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(append(os.Environ(), env...), extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		return "", fmt.Errorf("git %s %s: %w: %s", args[0], redact(url), err, msg)
	}
	return stdout.String(), nil
}

// env returns the environment that authenticates git to url. Secrets travel in
// the environment, not on the command line, so they don't show up in ps.
func (c *Cache) env(url string) ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
	var r *config.Repository
	if c.Credentials != nil {
		r = c.Credentials(url)
	}
	if r == nil {
		return env, nil
	}
	if r.PasswordFile != "" {
		pw, err := os.ReadFile(r.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("reading credentials for %s: %w", redact(url), err)
		}
		auth := base64.StdEncoding.EncodeToString([]byte(r.Username + ":" + strings.TrimSpace(string(pw))))
		env = append(env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	if r.SSHKeyFile != "" || r.KnownHostsFile != "" || r.InsecureIgnoreHostKey {
		ssh := []string{"ssh", "-o", "BatchMode=yes"}
		if r.SSHKeyFile != "" {
			ssh = append(ssh, "-i", shellQuote(r.SSHKeyFile), "-o", "IdentitiesOnly=yes")
		}
		switch {
		case r.InsecureIgnoreHostKey:
			ssh = append(ssh, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
		case r.KnownHostsFile != "":
			ssh = append(ssh, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+shellQuote(r.KnownHostsFile))
		}
		env = append(env, "GIT_SSH_COMMAND="+strings.Join(ssh, " "))
	}
	return env, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var userinfoRegexp = regexp.MustCompile(`://[^/@]*@`)

// redact removes any user:password from a URL for messages.
func redact(url string) string { return userinfoRegexp.ReplaceAllString(url, "://***@") }
