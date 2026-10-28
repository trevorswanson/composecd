package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Bootstrap is the server's own configuration file (composecd.yaml). It says
// where the root of the GitOps repository is and holds what must not live in
// Git: credentials and local paths. Everything else is an object in the root.
type Bootstrap struct {
	// Listen is the HTTP listen address (default ":8080").
	Listen string `yaml:"listen,omitempty"`
	// DataDir holds the database, Git cache and admin token (default /var/lib/composecd).
	DataDir string `yaml:"dataDir,omitempty"`
	// RefreshInterval is how often Git and every app are refreshed (default 3m).
	RefreshInterval Duration `yaml:"refreshInterval,omitempty"`
	// Concurrency bounds how many apps are reconciled at once (default 4).
	Concurrency int `yaml:"concurrency,omitempty"`
	// Root is the directory in Git that holds the Host and Application objects.
	Root Root `yaml:"root"`
	// Repositories hold credentials for Git repositories, matched by URL prefix.
	Repositories []Repository `yaml:"repositories,omitempty"`
	Auth         Auth         `yaml:"auth,omitempty"`
}

// Root locates the objects in Git.
type Root struct {
	RepoURL        string `yaml:"repoURL" json:"repoURL"`
	TargetRevision string `yaml:"targetRevision,omitempty" json:"targetRevision,omitempty"`
	Path           string `yaml:"path,omitempty" json:"path,omitempty"`
}

// Repository holds credentials for every Git URL starting with URL. Secrets are
// read from files, so composecd.yaml itself holds none.
type Repository struct {
	URL string `yaml:"url"`
	// Username and PasswordFile authenticate over HTTPS (a token works as the password).
	Username     string `yaml:"username,omitempty"`
	PasswordFile string `yaml:"passwordFile,omitempty"`
	// SSHKeyFile and KnownHostsFile authenticate over SSH.
	SSHKeyFile     string `yaml:"sshKeyFile,omitempty"`
	KnownHostsFile string `yaml:"knownHostsFile,omitempty"`
	// InsecureIgnoreHostKey skips SSH host key checking.
	InsecureIgnoreHostKey bool `yaml:"insecureIgnoreHostKey,omitempty"`
}

// Auth controls API authentication.
type Auth struct {
	// TokenFile holds the admin API token. When empty, composecd generates one in
	// <dataDir>/admin-token on first start.
	TokenFile string `yaml:"tokenFile,omitempty"`
	// Disabled turns authentication off, e.g. behind a proxy that authenticates.
	Disabled bool `yaml:"disabled,omitempty"`
}

// Defaults for Bootstrap.
const (
	DefaultListen          = ":8080"
	DefaultDataDir         = "/var/lib/composecd"
	DefaultRefreshInterval = 3 * time.Minute
	DefaultConcurrency     = 4
)

// LoadBootstrap reads and validates a bootstrap file, filling in defaults.
func LoadBootstrap(file string) (*Bootstrap, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	b, err := ParseBootstrap(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return b, nil
}

// ParseBootstrap parses and validates a bootstrap file, filling in defaults.
func ParseBootstrap(data []byte) (*Bootstrap, error) {
	b := &Bootstrap{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(b); err != nil {
		return nil, err
	}
	if b.Listen == "" {
		b.Listen = DefaultListen
	}
	if b.DataDir == "" {
		b.DataDir = DefaultDataDir
	}
	if b.RefreshInterval == 0 {
		b.RefreshInterval = Duration(DefaultRefreshInterval)
	}
	if b.Concurrency == 0 {
		b.Concurrency = DefaultConcurrency
	}
	if b.Root.TargetRevision == "" {
		b.Root.TargetRevision = "HEAD"
	}

	var errs []error
	if b.Root.RepoURL == "" {
		errs = append(errs, errors.New("root.repoURL is required"))
	}
	if p, err := cleanRelative(b.Root.Path); err != nil {
		errs = append(errs, fmt.Errorf("root.path %w", err))
	} else {
		b.Root.Path = p
	}
	if b.RefreshInterval < Duration(10*time.Second) {
		errs = append(errs, errors.New("refreshInterval must be at least 10s"))
	}
	if b.Concurrency < 1 {
		errs = append(errs, errors.New("concurrency must be at least 1"))
	}
	for i, r := range b.Repositories {
		if r.URL == "" {
			errs = append(errs, fmt.Errorf("repositories[%d].url is required", i))
		}
		if r.PasswordFile != "" && r.SSHKeyFile != "" {
			errs = append(errs, fmt.Errorf("repositories[%d]: set passwordFile or sshKeyFile, not both", i))
		}
	}
	return b, errors.Join(errs...)
}

// Credentials returns the repository entry with the longest URL prefix of url,
// or nil.
func (b *Bootstrap) Credentials(url string) *Repository {
	var best *Repository
	for i := range b.Repositories {
		r := &b.Repositories[i]
		if strings.HasPrefix(url, r.URL) && (best == nil || len(r.URL) > len(best.URL)) {
			best = r
		}
	}
	return best
}

// Resolve fills in each Application's defaults that depend on the root: an
// empty repoURL means the root repository, and an empty targetRevision means
// the root's revision for it (HEAD for any other repository).
func (o *Objects) Resolve(root Root) {
	for _, a := range o.Applications {
		s := &a.Spec.Source
		if s.IsOCI() {
			continue
		}
		if s.RepoURL == "" {
			s.RepoURL = root.RepoURL
		}
		if s.TargetRevision == "" {
			if s.RepoURL == root.RepoURL {
				s.TargetRevision = root.TargetRevision
			} else {
				s.TargetRevision = "HEAD"
			}
		}
	}
}
