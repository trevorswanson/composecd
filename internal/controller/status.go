package controller

import (
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/compose"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
)

// SyncState says whether the live state matches Git.
type SyncState string

const (
	Synced    SyncState = "Synced"
	OutOfSync SyncState = "OutOfSync"
	Unknown   SyncState = "Unknown"
)

// AppStatus is an Application's desired and live state as of its last reconcile.
type AppStatus struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Host   string            `json:"host"`
	Source config.AppSource  `json:"source"`
	// Origin is where the Application is defined in the root (file:line).
	Origin    string `json:"origin,omitempty"`
	Automated bool   `json:"automated"`
	// Paused stops automatic sync, e.g. after a rollback.
	Paused       bool         `json:"paused,omitempty"`
	PausedReason string       `json:"pausedReason,omitempty"`
	Sync         SyncStatus   `json:"sync"`
	Health       HealthStatus `json:"health"`
	Release      *Release     `json:"release,omitempty"`
	// DependsOn are the apps that must be Synced and Healthy first.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Blocked says why automation is holding off: a sync window, a dependency,
	// host maintenance, or a retry or self-heal backoff.
	Blocked string `json:"blocked,omitempty"`
	// Retry is the state of retries after a failed automatic sync.
	Retry *RetryStatus `json:"retry,omitempty"`
	// Operation is the newest operation (without its log).
	Operation *store.Operation `json:"operation,omitempty"`
	// Error is why the app could not be fully reconciled.
	Error        string    `json:"error,omitempty"`
	ReconciledAt time.Time `json:"reconciledAt"`
}

// SyncStatus compares Git with what is deployed.
type SyncStatus struct {
	Status SyncState `json:"status"`
	// Revision is the commit Git currently points to.
	Revision string `json:"revision,omitempty"`
	// DeployedRevision is the commit the deployed release was synced from.
	DeployedRevision string `json:"deployedRevision,omitempty"`
	// Reasons say why the app is out of sync.
	Reasons []string `json:"reasons,omitempty"`
	// Changes are the compose resources that differ between the deployed
	// manifest and Git.
	Changes []Change `json:"changes,omitempty"`
	// Drift is how the live containers differ from the deployed manifest.
	Drift []action.ServiceDrift `json:"drift,omitempty"`
}

// Change is one compose resource (services/web, networks/proxy, ...) that
// differs from Git.
type Change struct {
	Resource string `json:"resource"`
	Op       string `json:"op"`
	// Diff is a unified diff from the deployed to the desired YAML.
	Diff string `json:"diff"`
}

// HealthStatus is the app's health from its live containers.
type HealthStatus struct {
	Status     health.Status       `json:"status"`
	Message    string              `json:"message,omitempty"`
	Containers []compose.Container `json:"containers,omitempty"`
}

// Release summarizes the newest composetpl revision of the app's release.
type Release struct {
	Revision     int               `json:"revision"`
	Status       string            `json:"status"`
	Description  string            `json:"description,omitempty"`
	Chart        string            `json:"chart"`
	AppVersion   string            `json:"appVersion,omitempty"`
	LastDeployed time.Time         `json:"lastDeployed"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// RootStatus is the state of the Git root holding the objects.
type RootStatus struct {
	config.Root
	// Revision is the commit the objects were last read from.
	Revision    string    `json:"revision,omitempty"`
	Error       string    `json:"error,omitempty"`
	RefreshedAt time.Time `json:"refreshedAt,omitempty"`
}

// RetryStatus counts failed automatic syncs of one desired state.
type RetryStatus struct {
	Failures int `json:"failures"`
	Limit    int `json:"limit"`
	// NextAt is when the next retry runs (zero when retries are used up).
	NextAt time.Time `json:"nextAt,omitempty"`
}

// HostStatus is a host and how many apps target it.
type HostStatus struct {
	hosts.Status
	Apps int `json:"apps"`
	// Maintenance stops automatic syncs, self-heal and prune on the host.
	Maintenance       bool   `json:"maintenance,omitempty"`
	MaintenanceReason string `json:"maintenanceReason,omitempty"`
	// Orphans are releases composecd deployed whose Application is no longer in
	// Git for this host, and that weren't pruned (prune off, or blocked).
	Orphans []string `json:"orphans,omitempty"`
}

// Event is something notifications report.
type Event struct {
	// Type is one of the config.Event* names.
	Type    string    `json:"type"`
	App     string    `json:"app,omitempty"`
	Host    string    `json:"host,omitempty"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
	// Revision is the Git commit involved, if any.
	Revision string `json:"revision,omitempty"`
}

// Notifier receives events. It must not block.
type Notifier interface {
	Notify(Event)
}

// Labels composecd records on the releases it deploys.
const (
	LabelManagedBy      = "managed-by"
	ManagedBy           = "composecd"
	LabelApp            = "composecd/app"
	LabelSourceRevision = "composecd/source-revision"
	LabelOperationID    = "composecd/operation-id"
	// LabelPrune records that the app allowed prune when it was synced, so a
	// release can be pruned after its Application is gone from Git.
	LabelPrune = "composecd/prune"
)
