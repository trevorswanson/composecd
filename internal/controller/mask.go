package controller

import (
	"errors"

	"github.com/trevorswanson/composetpl/pkg/release"
	"github.com/trevorswanson/composetpl/pkg/storage"

	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/masking"
)

// remember retains old values for the lifetime of the controller: a rotated
// secret can still appear in a diff, historical output or an old container.
func (c *Controller) remember(name string, secrets []string) {
	c.maskMu.Lock()
	defer c.maskMu.Unlock()
	if c.secretValues == nil {
		c.secretValues = map[string]map[string]bool{}
	}
	if c.secretValues[name] == nil {
		c.secretValues[name] = map[string]bool{}
	}
	for _, s := range secrets {
		c.secretValues[name][s] = true
	}
}

func (c *Controller) masker(name string) *masking.Masker {
	c.maskMu.RLock()
	secrets := make([]string, 0, len(c.secretValues[name]))
	for s := range c.secretValues[name] {
		secrets = append(secrets, s)
	}
	c.maskMu.RUnlock()
	if app, err := c.application(name); err == nil {
		secrets = append(secrets, masking.Leaves(app.Spec.Source.Values)...)
		secrets = append(secrets, masking.Leaves(app.Spec.Source.Env)...)
		if h := c.Hosts.Get(app.Spec.Destination.Host); h != nil {
			secrets = append(secrets, masking.Leaves(h.Spec.Spec.Env)...)
		}
	}
	return masking.New(secrets)
}

// Mask hides decrypted and configured values in human-readable output.
func (c *Controller) Mask(name, text string) string { return c.masker(name).Text(text) }

// masked redacts a detached public value. Internal manifests, source values and
// desired-state keys stay untouched so masking cannot change a deployment.
func masked[T any](c *Controller, name string, in T) T {
	var out T
	if err := c.masker(name).Redact(in, &out); err != nil {
		c.Log.Error("masking output failed", "app", name)
		return out // fail closed, never return unredacted data
	}
	return out
}

// releaseHistory recovers user values from every retained revision. This also
// restores masking after restart, during rollback and with a missing age key.
// The host's own release records remain plaintext, as composetpl requires.
func (c *Controller) releaseHistory(h *hosts.Host, name string) ([]*release.Release, error) {
	hist, err := h.Store().History(name)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, r := range hist {
		c.remember(name, masking.Leaves(r.Config))
	}
	return hist, nil
}

// maskStatus keeps routing identifiers and state enums stable even when a
// decrypted value happens to equal an application/container name or a state.
func (c *Controller) maskStatus(st AppStatus) AppStatus {
	out := masked(c, st.Name, st)
	out.Name, out.Host = st.Name, st.Host
	out.Source.Values = c.masker(st.Name).Values(st.Source.Values)
	out.Sync.Status, out.Health.Status = st.Sync.Status, st.Health.Status
	out.Sync.Revision, out.Sync.DeployedRevision = st.Sync.Revision, st.Sync.DeployedRevision
	out.Sync.ValuesRevision, out.Sync.DeployedValuesRevision = st.Sync.ValuesRevision, st.Sync.DeployedValuesRevision
	out.Sync.ChartRef = st.Sync.ChartRef
	for i, ct := range st.Health.Containers {
		if i >= len(out.Health.Containers) {
			break
		}
		out.Health.Containers[i].Name = ct.Name
		out.Health.Containers[i].Service = ct.Service
	}
	if st.Operation != nil && out.Operation != nil {
		out.Operation.App = st.Operation.App
		out.Operation.User = st.Operation.User
		out.Operation.Kind = st.Operation.Kind
		out.Operation.Phase = st.Operation.Phase
		out.Operation.Trigger = st.Operation.Trigger
	}
	if st.Release != nil && out.Release != nil {
		out.Release.Status = st.Release.Status
		out.Release.ChartRef = st.Release.ChartRef
	}
	return out
}
