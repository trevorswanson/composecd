package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	// Webhook lets Git servers trigger a refresh on push.
	Webhook Webhook `yaml:"webhook,omitempty"`
	// Notifications send events (failed syncs, degraded apps, ...) elsewhere.
	Notifications []Notifier `yaml:"notifications,omitempty"`
	// SOPSAgeKeyFile is a mounted age identity used only by the SOPS subprocess.
	SOPSAgeKeyFile string `yaml:"sopsAgeKeyFile,omitempty"`
	// Infisical lets Applications read secrets from Infisical.
	Infisical *Infisical `yaml:"infisical,omitempty"`
	// ImageUpdates pins services' moving tags to registry digests, checked on
	// a schedule, so a new image shows as a change from Git.
	ImageUpdates ImageUpdates `yaml:"imageUpdates,omitempty"`

	// Path is the absolute path the file was read from, when LoadBootstrap
	// read it.
	Path string `yaml:"-" json:"-"`
}

// ImageUpdates configures image update checks. Each Application and each
// service can opt in or out; this is the default for the rest.
type ImageUpdates struct {
	// Enabled makes every Application take part unless it opts out.
	Enabled bool `yaml:"enabled,omitempty"`
	// Schedule is a cron expression for when registries are checked (default
	// hourly). It runs whenever any Application takes part.
	Schedule string `yaml:"schedule,omitempty"`
	// TimeZone is an IANA zone for Schedule (default UTC).
	TimeZone string `yaml:"timeZone,omitempty"`
}

// DefaultImageSchedule checks registries hourly.
const DefaultImageSchedule = "0 * * * *"

// Infisical connects composecd to an Infisical server. Set clientID with
// clientSecretFile (a machine identity using Universal Auth), or tokenFile
// (an access token). Credentials are read from files, like all other secrets.
type Infisical struct {
	// SiteURL is the Infisical server (default https://app.infisical.com).
	SiteURL          string `yaml:"siteURL,omitempty"`
	ClientID         string `yaml:"clientID,omitempty"`
	ClientSecretFile string `yaml:"clientSecretFile,omitempty"`
	TokenFile        string `yaml:"tokenFile,omitempty"`
}

// Webhook configures POST /api/v1/webhook.
type Webhook struct {
	// SecretFile holds the shared secret Git servers sign payloads with
	// (GitHub, Gitea and Forgejo) or send as a token (GitLab). Without it the
	// endpoint is off.
	SecretFile string `yaml:"secretFile,omitempty"`
}

// Notifier sends events to one service.
type Notifier struct {
	Name string `yaml:"name"`
	// Type is webhook (JSON POST), ntfy, gotify, discord or slack.
	Type string `yaml:"type"`
	// URL is the endpoint: an ntfy topic URL, a Gotify server, a Discord or
	// Slack webhook URL, or any URL for webhook. URLFile reads it from a file
	// instead, for URLs that are secrets.
	URL     string `yaml:"url,omitempty"`
	URLFile string `yaml:"urlFile,omitempty"`
	// TokenFile holds an ntfy access token or a Gotify application token.
	TokenFile string `yaml:"tokenFile,omitempty"`
	// Events to send (default: sync-failed, health-degraded, host-down, prune).
	Events []string `yaml:"events,omitempty"`
	// Applications are name globs; events about other apps aren't sent
	// (default: every app). Host events are always sent.
	Applications []string `yaml:"applications,omitempty"`
}

// Notification events.
const (
	EventSyncSucceeded  = "sync-succeeded"
	EventSyncFailed     = "sync-failed"
	EventHealed         = "self-healed"
	EventPruned         = "prune"
	EventHealthDegraded = "health-degraded"
	EventOutOfSync      = "out-of-sync"
	EventHostDown       = "host-down"
	// EventImageUpdate is a new digest for an image tag a service uses.
	EventImageUpdate = "image-update"
)

// AllEvents lists every notification event.
var AllEvents = []string{EventSyncSucceeded, EventSyncFailed, EventHealed, EventPruned, EventHealthDegraded, EventOutOfSync, EventHostDown, EventImageUpdate}

// DefaultEvents are sent when a notifier lists none.
var DefaultEvents = []string{EventSyncFailed, EventHealthDegraded, EventHostDown, EventPruned}

// Notifier types.
var notifierTypes = map[string]bool{"webhook": true, "ntfy": true, "gotify": true, "discord": true, "slack": true}

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
	// OIDC enables signing in with an OpenID Connect provider.
	OIDC *OIDC `yaml:"oidc,omitempty"`
}

// OIDC configures sign-in with an OpenID Connect provider (Authentik,
// Authelia, Keycloak, Pocket ID, ...), with the authorization code flow and PKCE.
type OIDC struct {
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"clientID"`
	// ClientSecretFile holds the client secret; omit it for a public client.
	ClientSecretFile string `yaml:"clientSecretFile,omitempty"`
	// RedirectURL is composecd's callback, e.g. https://composecd.lan/auth/callback.
	RedirectURL string   `yaml:"redirectURL"`
	Scopes      []string `yaml:"scopes,omitempty"`
	// UsernameClaim names the user (default preferred_username, then email, then sub).
	UsernameClaim string `yaml:"usernameClaim,omitempty"`
	// GroupsClaim lists the user's groups (default groups).
	GroupsClaim string `yaml:"groupsClaim,omitempty"`
	// AdminGroups get the admin role.
	AdminGroups []string `yaml:"adminGroups,omitempty"`
	// ReadonlyGroups get the readonly role. When empty, every other user who
	// signs in is readonly; list groups to restrict who may sign in at all.
	ReadonlyGroups []string `yaml:"readonlyGroups,omitempty"`
}

// Defaults for Bootstrap.
const (
	DefaultListen          = ":8080"
	DefaultDataDir         = "/var/lib/composecd"
	DefaultRefreshInterval = 3 * time.Minute
	DefaultConcurrency     = 4

	DefaultInfisicalSiteURL = "https://app.infisical.com"
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
	if b.Path, err = filepath.Abs(file); err != nil {
		return nil, err
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
	if b.ImageUpdates.Schedule == "" {
		b.ImageUpdates.Schedule = DefaultImageSchedule
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
	if _, err := ParseSchedule(b.ImageUpdates.Schedule); err != nil {
		errs = append(errs, fmt.Errorf("imageUpdates.schedule: %w", err))
	}
	if _, err := time.LoadLocation(b.ImageUpdates.TimeZone); err != nil {
		errs = append(errs, fmt.Errorf("imageUpdates.timeZone %q: %w", b.ImageUpdates.TimeZone, err))
	}
	for i, r := range b.Repositories {
		if r.URL == "" {
			errs = append(errs, fmt.Errorf("repositories[%d].url is required", i))
		}
		if r.PasswordFile != "" && r.SSHKeyFile != "" {
			errs = append(errs, fmt.Errorf("repositories[%d]: set passwordFile or sshKeyFile, not both", i))
		}
	}
	if o := b.Auth.OIDC; o != nil {
		if b.Auth.Disabled {
			errs = append(errs, errors.New("auth.oidc and auth.disabled can't both be set"))
		}
		if !strings.HasPrefix(o.Issuer, "https://") && !strings.HasPrefix(o.Issuer, "http://localhost") && !strings.HasPrefix(o.Issuer, "http://127.0.0.1") {
			errs = append(errs, errors.New("auth.oidc.issuer must be an https:// URL"))
		}
		if o.ClientID == "" {
			errs = append(errs, errors.New("auth.oidc.clientID is required"))
		}
		if u, err := url.Parse(o.RedirectURL); err != nil || u.Scheme == "" || u.Host == "" || u.Path == "" || u.Path == "/" {
			errs = append(errs, errors.New("auth.oidc.redirectURL must be an absolute URL with a path, e.g. https://composecd.lan/auth/callback"))
		}
		if len(o.AdminGroups) == 0 {
			errs = append(errs, errors.New("auth.oidc.adminGroups needs at least one group"))
		}
	}
	if i := b.Infisical; i != nil {
		if i.SiteURL == "" {
			i.SiteURL = DefaultInfisicalSiteURL
		}
		if !hasScheme(i.SiteURL, "https://") && !strings.HasPrefix(i.SiteURL, "http://localhost") && !strings.HasPrefix(i.SiteURL, "http://127.0.0.1") {
			errs = append(errs, errors.New("infisical.siteURL must be an https:// URL"))
		}
		switch {
		case i.TokenFile != "" && (i.ClientID != "" || i.ClientSecretFile != ""):
			errs = append(errs, errors.New("infisical: set tokenFile, or clientID with clientSecretFile, not both"))
		case i.TokenFile == "" && (i.ClientID == "" || i.ClientSecretFile == ""):
			errs = append(errs, errors.New("infisical: set tokenFile, or both clientID and clientSecretFile"))
		}
	}
	names := map[string]bool{}
	for i, n := range b.Notifications {
		where := fmt.Sprintf("notifications[%d]", i)
		if n.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name is required", where))
		} else if names[n.Name] {
			errs = append(errs, fmt.Errorf("%s: name %q is used twice", where, n.Name))
		}
		names[n.Name] = true
		if !notifierTypes[n.Type] {
			errs = append(errs, fmt.Errorf("%s: type %q must be webhook, ntfy, gotify, discord or slack", where, n.Type))
		}
		if (n.URL == "") == (n.URLFile == "") {
			errs = append(errs, fmt.Errorf("%s: set url or urlFile", where))
		}
		for _, e := range n.Events {
			if !contains(AllEvents, e) {
				errs = append(errs, fmt.Errorf("%s: unknown event %q (want one of %s)", where, e, strings.Join(AllEvents, ", ")))
			}
		}
	}
	return b, errors.Join(errs...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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

// Waves orders the Applications so each comes after everything it dependsOn:
// wave 0 depends on nothing, wave 1 only on wave 0, and so on. Objects must be
// valid (no cycles).
func (o *Objects) Waves() [][]*Application {
	level := map[string]int{}
	var depth func(a *Application) int
	depth = func(a *Application) int {
		if l, ok := level[a.Metadata.Name]; ok {
			return l
		}
		level[a.Metadata.Name] = 0 // a cycle (invalid objects) ends here
		l := 0
		for _, d := range a.Spec.DependsOn {
			if dep := o.Application(d); dep != nil {
				l = max(l, depth(dep)+1)
			}
		}
		level[a.Metadata.Name] = l
		return l
	}
	var waves [][]*Application
	for _, a := range o.Applications {
		l := depth(a)
		for len(waves) <= l {
			waves = append(waves, nil)
		}
		waves[l] = append(waves[l], a)
	}
	return waves
}

// Resolve fills in each Application's defaults that depend on the root: an
// empty repoURL means the root repository, and an empty targetRevision means
// the root's revision for it (HEAD for any other repository).
func (o *Objects) Resolve(root Root) {
	resolve := func(url, revision *string) {
		if *url == "" {
			*url = root.RepoURL
		}
		if *revision == "" {
			if *url == root.RepoURL {
				*revision = root.TargetRevision
			} else {
				*revision = "HEAD"
			}
		}
	}
	for _, set := range o.ApplicationSets {
		for _, g := range set.Spec.Generators {
			if d := g.GitDirectories; d != nil {
				resolve(&d.RepoURL, &d.TargetRevision)
			}
		}
	}
	for _, a := range o.Applications {
		s := &a.Spec.Source
		if !s.IsOCI() {
			resolve(&s.RepoURL, &s.TargetRevision)
		}
		if s.ValuesFrom != nil {
			resolve(&s.ValuesFrom.RepoURL, &s.ValuesFrom.TargetRevision)
		}
	}
}
