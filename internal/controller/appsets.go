package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/appset"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/masking"
)

// ApplicationSetStatus describes generator output without exposing templates.
type ApplicationSetStatus = appset.Status

func (c *Controller) ApplicationSets() []ApplicationSetStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ApplicationSetStatus, len(c.appsets))
	for i, s := range c.appsets {
		out[i] = s
		out[i].Applications = append([]string{}, s.Applications...)
	}
	return out
}

// expand adds each ApplicationSet's Applications to objs. A set that fails, or
// suddenly generates nothing, keeps the Applications it generated before and
// reports the error in its status, so one broken set doesn't hold back the
// rest of Git. Only a failed validation of the whole snapshot returns an error.
func (c *Controller) expand(ctx context.Context, objs *config.Objects) (*config.Objects, []ApplicationSetStatus, map[string]error, error) {
	c.mu.RLock()
	previous := map[string][]*config.Application{}
	if c.objects != nil {
		for _, a := range c.objects.Applications {
			if owner := a.Metadata.Labels[config.LabelApplicationSet]; owner != "" {
				previous[owner] = append(previous[owner], a)
			}
		}
	}
	c.mu.RUnlock()
	fetched := map[string]error{c.Boot.Root.RepoURL: nil}
	checkout := func(ctx context.Context, url, revision string) (string, func(), error) {
		if _, ok := fetched[url]; !ok {
			fetched[url] = c.Git.Fetch(ctx, url)
		}
		if err := fetched[url]; err != nil {
			return "", nil, fmt.Errorf("fetching generator repository %s: %w", url, err)
		}
		sha, err := c.Git.Resolve(ctx, url, revision)
		if err != nil {
			return "", nil, err
		}
		done := c.useCheckout(url, sha)
		dir, err := c.Git.Checkout(ctx, url, sha)
		if err != nil {
			done()
			return "", nil, err
		}
		return dir, done, nil
	}
	expanded := *objs
	expanded.Applications = append([]*config.Application{}, objs.Applications...)
	statuses := make([]ApplicationSetStatus, 0, len(objs.ApplicationSets))
	for _, set := range objs.ApplicationSets {
		apps, err := appset.Generate(ctx, set, objs.Hosts, checkout)
		kept := previous[set.Metadata.Name]
		st := ApplicationSetStatus{Name: set.Metadata.Name, Origin: set.Origin, Generated: len(apps), Previous: len(kept), Applications: []string{}}
		if err == nil && len(apps) == 0 && st.Previous > 0 {
			err = fmt.Errorf("generated: 0, was: %d; keeping the applications it generated before", st.Previous)
		}
		if err != nil {
			m := masking.New(append(masking.Leaves(set.Spec.Template.Spec.Source.Values), masking.Leaves(set.Spec.Template.Spec.Source.Env)...))
			st.Error = m.Text(err.Error())
			apps = kept
		}
		for _, a := range apps {
			st.Applications = append(st.Applications, a.Metadata.Name)
		}
		expanded.Applications = append(expanded.Applications, apps...)
		sort.Strings(st.Applications)
		statuses = append(statuses, st)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	expanded.Resolve(c.Boot.Root)
	if err := expanded.Validate(); err != nil {
		for i := range statuses {
			if statuses[i].Error == "" && strings.Contains(err.Error(), "applicationset "+statuses[i].Name+" (") {
				statuses[i].Error = err.Error()
			}
			if statuses[i].Error == "" {
				statuses[i].Error = "snapshot not applied: another object or ApplicationSet failed validation"
			}
		}
		return nil, statuses, fetched, err
	}
	for i := range statuses {
		statuses[i].Ready = statuses[i].Error == ""
	}
	return &expanded, statuses, fetched, nil
}

// setErrors reports the sets that failed without failing the snapshot.
func setErrors(statuses []ApplicationSetStatus) error {
	var errs []error
	for _, s := range statuses {
		if s.Error != "" {
			errs = append(errs, fmt.Errorf("applicationset %s: %s", s.Name, s.Error))
		}
	}
	return errors.Join(errs...)
}

// restoreEmptyGuards recovers previous counts from release ownership labels.
// Even a restarted controller must never prune a zero-result set's releases.
func (c *Controller) restoreEmptyGuards() {
	empty := map[string]bool{}
	for _, s := range c.ApplicationSets() {
		if s.Generated == 0 {
			empty[s.Name] = true
		}
	}
	if len(empty) == 0 {
		return
	}
	found := map[string]map[string]bool{}
	for _, h := range c.pruneHosts() {
		if h.Status().State != hosts.StateUp {
			continue
		}
		func() {
			defer h.Use()()
			rels, err := h.Store().List()
			if err != nil {
				return
			}
			for _, r := range rels {
				owner := r.Labels[config.LabelApplicationSet]
				if !empty[owner] || r.Labels[LabelManagedBy] != ManagedBy || r.Status == release.StatusUninstalled {
					continue
				}
				if found[owner] == nil {
					found[owner] = map[string]bool{}
				}
				found[owner][h.Name()+"/"+r.Name] = true
			}
		}()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.appsets {
		if n := len(found[s.Name]); s.Generated == 0 && n > 0 {
			if n > s.Previous {
				c.appsets[i].Previous = n
			}
			if s.Error == "" {
				c.appsets[i].Error = fmt.Sprintf("generated: 0, was: %d; protecting existing releases", n)
			}
			c.appsets[i].Ready = false
			message := "applicationset " + s.Name + ": " + c.appsets[i].Error
			if !strings.Contains(c.root.Error, message) {
				if c.root.Error != "" {
					c.root.Error += "; "
				}
				c.root.Error += message
			}
		}
	}
}

// setPruneGuard says why an ApplicationSet's releases must not be pruned: the
// set failed or generated nothing, so its missing apps prove nothing.
func (c *Controller) setPruneGuard(owner string) string {
	if owner == "" {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.appsets {
		if s.Name != owner {
			continue
		}
		if s.Error != "" {
			return "applicationset " + owner + " failed: " + s.Error
		}
		if s.Generated == 0 {
			return fmt.Sprintf("applicationset %s generated: 0, was: %d", owner, s.Previous)
		}
	}
	return ""
}

// Removed hosts remain reachable for prune/retries during this process, until
// they are reintroduced. Their specs are not persisted outside Git.
func (c *Controller) pruneHosts() []*hosts.Host {
	out := c.Hosts.All()
	c.mu.RLock()
	for _, h := range c.retiredHosts {
		out = append(out, h)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
