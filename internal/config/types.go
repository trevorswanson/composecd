// Package config defines composecd's objects (Host, Application) as they are
// written in Git, the bootstrap file the server starts from, and their loading
// and validation.
package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// APIVersion is the apiVersion of every composecd object.
const APIVersion = "composecd/v1alpha1"

// Object kinds.
const (
	KindHost        = "Host"
	KindApplication = "Application"
)

// Metadata names an object.
type Metadata struct {
	Name   string            `yaml:"name" json:"name"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// Host is a Docker engine composecd deploys to.
type Host struct {
	Metadata Metadata `yaml:"metadata" json:"metadata"`
	Spec     HostSpec `yaml:"spec" json:"spec"`
	// Origin is where the object was defined (file:line).
	Origin string `yaml:"-" json:"origin,omitempty"`
}

// HostSpec says how to reach an engine and where releases live on it.
type HostSpec struct {
	// URL is the engine address, as for `docker -H`: unix:///var/run/docker.sock,
	// ssh://user@host or tcp://host:2376. Empty uses the controller's default
	// engine (DOCKER_HOST, or the local socket).
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Context is a docker context name, instead of URL.
	Context string `yaml:"context,omitempty" json:"context,omitempty"`
	// TLSCertPath is a directory on the controller with ca.pem, cert.pem and
	// key.pem for a tcp:// engine (DOCKER_CERT_PATH with DOCKER_TLS_VERIFY=1).
	TLSCertPath string `yaml:"tlsCertPath,omitempty" json:"tlsCertPath,omitempty"`
	// ProjectRoot is the directory, on the engine's machine, that each app's
	// compose project directory is created under (<projectRoot>/<app>).
	// Relative paths in compose files, such as ./data bind mounts, resolve there.
	ProjectRoot string `yaml:"projectRoot,omitempty" json:"projectRoot,omitempty"`
	// Storage is where composetpl keeps release history: "volume" (default, on
	// the engine) or "local" (in the controller's data directory).
	Storage string `yaml:"storage,omitempty" json:"storage,omitempty"`
	// Env is added to the environment of every docker call to this host, and is
	// visible to compose ${VAR} interpolation and the env template function.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Defaults for HostSpec.
const (
	DefaultProjectRoot = "/opt/composecd"
	StorageVolume      = "volume"
	StorageLocal       = "local"
)

// Application is a chart deployed as one composetpl release on one host. The
// release (and compose project) name is the Application's name.
type Application struct {
	Metadata Metadata `yaml:"metadata" json:"metadata"`
	Spec     AppSpec  `yaml:"spec" json:"spec"`
	// Origin is where the object was defined (file:line).
	Origin string `yaml:"-" json:"origin,omitempty"`
}

// AppSpec is the desired state of an Application.
type AppSpec struct {
	Source      AppSource   `yaml:"source" json:"source"`
	Destination Destination `yaml:"destination" json:"destination"`
	SyncPolicy  SyncPolicy  `yaml:"syncPolicy,omitempty" json:"syncPolicy"`
}

// AppSource is where an Application's chart and values come from: a chart
// directory in a Git repository (Path), or an oci:// chart (Chart).
type AppSource struct {
	// RepoURL is the Git repository holding the chart. Empty means the root
	// repository the Application itself was read from.
	RepoURL string `yaml:"repoURL,omitempty" json:"repoURL,omitempty"`
	// TargetRevision is a branch, tag or commit. Empty means the root's revision
	// for the root repository, and HEAD for any other.
	TargetRevision string `yaml:"targetRevision,omitempty" json:"targetRevision,omitempty"`
	// Path is the chart directory inside the repository.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// Chart is an oci:// chart reference, instead of RepoURL and Path.
	Chart string `yaml:"chart,omitempty" json:"chart,omitempty"`
	// Version is a semver constraint for Chart.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// ValueFiles are values files relative to Path (they may use .. to reach
	// other directories of the same repository), applied in order over the
	// chart's values.yaml.
	ValueFiles []string `yaml:"valueFiles,omitempty" json:"valueFiles,omitempty"`
	// Values are applied last, over ValueFiles.
	Values map[string]interface{} `yaml:"values,omitempty" json:"values,omitempty"`
	// Env is visible to compose ${VAR} interpolation and the env template
	// function, over the host's Env.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// IsOCI reports whether the source is an oci:// chart rather than a Git path.
func (s AppSource) IsOCI() bool { return s.Chart != "" }

// Destination is where an Application runs.
type Destination struct {
	Host string `yaml:"host" json:"host"`
}

// SyncPolicy controls when and how an Application is synced.
type SyncPolicy struct {
	// Automated, when set, syncs the Application whenever it is out of sync.
	Automated *Automated  `yaml:"automated,omitempty" json:"automated,omitempty"`
	Options   SyncOptions `yaml:"options,omitempty" json:"options"`
}

// Automated configures automatic sync.
type Automated struct{}

// SyncOptions map to composetpl upgrade flags.
type SyncOptions struct {
	// Wait waits until services are running and healthy (default true).
	Wait *bool `yaml:"wait,omitempty" json:"wait,omitempty"`
	// Timeout bounds Wait and each hook (default 5m).
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Pull is the image pull policy: always, missing or never (default: compose's).
	Pull string `yaml:"pull,omitempty" json:"pull,omitempty"`
	// Atomic rolls a failed sync back to the last deployed revision.
	Atomic bool `yaml:"atomic,omitempty" json:"atomic,omitempty"`
	// ForceRecreate recreates containers even if their configuration is unchanged.
	ForceRecreate bool `yaml:"forceRecreate,omitempty" json:"forceRecreate,omitempty"`
	// HistoryMax is how many revisions composetpl keeps (default 10, 0 = unlimited).
	HistoryMax *int `yaml:"historyMax,omitempty" json:"historyMax,omitempty"`
}

// Defaults for SyncOptions.
const (
	DefaultTimeout    = 5 * time.Minute
	DefaultHistoryMax = 10
)

// WaitOrDefault is Wait, defaulting to true.
func (o SyncOptions) WaitOrDefault() bool { return o.Wait == nil || *o.Wait }

// TimeoutOrDefault is Timeout, defaulting to DefaultTimeout.
func (o SyncOptions) TimeoutOrDefault() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return time.Duration(o.Timeout)
}

// HistoryMaxOrDefault is HistoryMax, defaulting to DefaultHistoryMax.
func (o SyncOptions) HistoryMaxOrDefault() int {
	if o.HistoryMax == nil {
		return DefaultHistoryMax
	}
	return *o.HistoryMax
}

// Duration is a time.Duration written as a Go duration string ("90s", "5m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) { return time.Duration(d).String(), nil }

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }
