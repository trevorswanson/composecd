package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trevorswanson/composecd/internal/appset"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/store"
)

// rolloutPrefix starts every reason a rolling sync gives for holding off, so
// a sibling's status change knows which apps to wake.
const rolloutPrefix = "rolling sync: "

// staleRecheck is how soon an app waiting for a sibling's reassessment looks
// again.
var staleRecheck = time.Second

// The states of an Application in a rolling sync.
const (
	RolloutDone        = "Done"
	RolloutPending     = "Pending"
	RolloutUpdating    = "Updating"
	RolloutStabilizing = "Stabilizing"
	RolloutUnhealthy   = "Unhealthy"
	RolloutFailed      = "Failed"
	RolloutUnknown     = "Unknown"
)

// reassessing is the message of a sibling whose status may predate the change.
const reassessing = "reassessing"

// rollout is an ApplicationSet's Applications grouped into its rolling sync's
// steps.
type rollout struct {
	set        *config.ApplicationSet
	minHealthy time.Duration
	steps      []rolloutStep
	unselected []string
}

type rolloutStep struct {
	max  int
	apps []string
}

// rolloutOfLocked returns the rolling sync of the ApplicationSet named owner,
// or nil if it has none. c.mu must be held.
func (c *Controller) rolloutOfLocked(owner string) *rollout {
	if owner == "" || c.objects == nil {
		return nil
	}
	i := slices.IndexFunc(c.objects.ApplicationSets, func(s *config.ApplicationSet) bool { return s.Metadata.Name == owner })
	if i < 0 {
		return nil
	}
	set := c.objects.ApplicationSets[i]
	if set.Spec.Strategy == nil || set.Spec.Strategy.RollingSync == nil {
		return nil
	}
	rs := set.Spec.Strategy.RollingSync
	steps := rs.Steps
	if len(steps) == 0 {
		steps = []config.RollingSyncStep{{}}
	}
	r := &rollout{set: set, minHealthy: time.Duration(rs.MinHealthyTime), steps: make([]rolloutStep, len(steps))}
	var apps []*config.Application
	for _, a := range c.objects.Applications {
		if a.Metadata.Labels[config.LabelApplicationSet] == owner {
			apps = append(apps, a)
		}
	}
	slices.SortFunc(apps, func(a, b *config.Application) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	for _, a := range apps {
		i := slices.IndexFunc(steps, func(s config.RollingSyncStep) bool { return s.Selects(a.Metadata.Labels) })
		if i < 0 {
			r.unselected = append(r.unselected, a.Metadata.Name)
			continue
		}
		r.steps[i].apps = append(r.steps[i].apps, a.Metadata.Name)
	}
	for i, s := range steps {
		m := s.MaxUpdate
		if m == nil {
			m = rs.MaxUpdate
		}
		r.steps[i].max = m.Of(len(r.steps[i].apps))
	}
	return r
}

// memberLocked returns where app stands in rollout r, why, and when that
// changes by itself (when it has been Healthy long enough). c.mu must be held.
func (c *Controller) memberLocked(r *rollout, name string, now time.Time) (state, message string, wake time.Time) {
	s := c.apps[name]
	if s == nil || s.status.Name == "" {
		return RolloutUnknown, "not reconciled yet", time.Time{}
	}
	st := s.status
	switch {
	case s.rollingOut || c.running[name] != 0:
		return RolloutUpdating, "syncing", time.Time{}
	case st.Sync.Status == OutOfSync && s.failures > 0 && st.Operation != nil && st.Operation.Phase == store.PhaseFailed:
		msg := "automatic sync failed"
		if op := st.Operation; op != nil && op.Message != "" {
			msg += ": " + op.Message
		}
		return RolloutFailed, msg, time.Time{}
	case st.Health.Status != health.Healthy:
		return RolloutUnhealthy, string(st.Health.Status), time.Time{}
	case st.Sync.Status != Synced:
		return RolloutPending, string(st.Sync.Status), time.Time{}
	}
	if until := s.healthySince.Add(r.minHealthy); now.Before(until) {
		return RolloutStabilizing, fmt.Sprintf("Healthy for %s of %s", now.Sub(s.healthySince).Round(time.Second), r.minHealthy), until
	}
	return RolloutDone, "", time.Time{}
}

// needsRepair says whether an Application in state is broken already, so
// syncing it can't make the set any less available.
func needsRepair(state string) bool {
	return state == RolloutUnhealthy || state == RolloutFailed || state == RolloutUnknown
}

// rolloutBlockLocked says why app's ApplicationSet's rolling sync holds back
// its automatic sync, or "", and when to look again if only time is needed.
// rolling is whether the app is in a rolling sync at all. c.mu must be held.
//
// An app syncs once every app of the earlier steps is Done, and while fewer
// than its step's maxUpdate other apps of the step are busy: updating,
// stabilizing, broken, or waiting to sync ahead of it. Broken apps sync first,
// so a fix in Git reaches them; among equals, apps sync in name order. A
// healthy app always waits for a broken sibling: updating it could take the
// last working copy down.
//
// A sibling assessed before generation gen may not know of the change this
// app is about to sync (both read the same commit), so it counts as busy and
// is reconciled now; its new status wakes this app again.
func (c *Controller) rolloutBlockLocked(app *config.Application, gen uint64, now time.Time) (reason string, wake time.Time, rolling bool) {
	r := c.rolloutOfLocked(app.Metadata.Labels[config.LabelApplicationSet])
	if r == nil {
		return "", time.Time{}, false
	}
	name := app.Metadata.Name
	soonest := func(t time.Time) {
		if !t.IsZero() && (wake.IsZero() || t.Before(wake)) {
			wake = t
		}
	}
	if slices.Contains(r.unselected, name) {
		return rolloutPrefix + "no step selects this application; sync it manually", time.Time{}, true
	}
	member := func(other string) (string, string, time.Time) {
		if s := c.apps[other]; s != nil && s.status.Name != "" && s.status.generation < gen {
			c.scheduleLocked(other, now)
			// Its new status wakes this app, unless this app's own status
			// lands after it: look again soon either way.
			soonest(now.Add(staleRecheck))
			return RolloutUnknown, reassessing, time.Time{}
		}
		return c.memberLocked(r, other, now)
	}
	for i, step := range r.steps {
		idx := slices.Index(step.apps, name)
		if idx < 0 {
			var waiting []string
			for _, other := range step.apps {
				state, msg, w := member(other)
				if state != RolloutDone {
					waiting = append(waiting, fmt.Sprintf("%s (%s)", other, msg))
					soonest(w)
				}
			}
			if len(waiting) > 0 {
				return fmt.Sprintf("%swaiting for step %d: %s", rolloutPrefix, i+1, listed(waiting)), wake, true
			}
			continue
		}
		if step.max == 0 {
			return fmt.Sprintf("%sstep %d is held (maxUpdate 0)", rolloutPrefix, i+1), time.Time{}, true
		}
		self, _, _ := c.memberLocked(r, name, now)
		repair := needsRepair(self)
		var busy []string
		for j, other := range step.apps {
			if j == idx {
				continue
			}
			state, msg, w := member(other)
			queued := c.pendingLocked(other)
			switch {
			case state == RolloutUpdating || state == RolloutStabilizing || state == RolloutUnknown:
				busy = append(busy, fmt.Sprintf("%s (%s)", other, msg))
				soonest(w)
			case needsRepair(state):
				// Syncing a broken app can't take a working copy down, so it
				// only waits for broken apps queued ahead of it.
				if !repair || (queued && j < idx) {
					busy = append(busy, fmt.Sprintf("%s (%s)", other, msg))
				}
			case state == RolloutPending && queued && !repair && j < idx:
				busy = append(busy, other+" (queued first)")
			}
		}
		if len(busy) >= step.max {
			return fmt.Sprintf("%sstep %d allows %d at a time: %s", rolloutPrefix, i+1, step.max, listed(busy)), wake, true
		}
		return "", time.Time{}, true
	}
	return "", time.Time{}, true
}

// pendingLocked says whether name is out of sync and will sync by itself: it
// is automated and not paused. c.mu must be held.
func (c *Controller) pendingLocked(name string) bool {
	s := c.apps[name]
	return s != nil && s.status.Sync.Status == OutOfSync && s.status.Automated && !s.status.Paused
}

// listed joins at most three items, saying how many more there are.
func listed(items []string) string {
	const n = 3
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

// endRollout releases app's place in its rollout once its status shows the
// sync, and lets the apps waiting on it look again.
func (c *Controller) endRollout(app *config.Application) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.apps[app.Metadata.Name]
	if s == nil || !s.rollingOut {
		return
	}
	s.rollingOut = false
	c.wakeRolloutLocked(app.Metadata.Labels[config.LabelApplicationSet], app.Metadata.Name)
}

// wakeRolloutLocked reconciles, shortly, the apps of owner's rolling sync that
// it holds back, except except. c.mu must be held.
func (c *Controller) wakeRolloutLocked(owner, except string) {
	r := c.rolloutOfLocked(owner)
	if r == nil {
		return
	}
	for _, step := range r.steps {
		for _, name := range step.apps {
			if s := c.apps[name]; name != except && s != nil && strings.HasPrefix(s.status.Blocked, rolloutPrefix) {
				c.scheduleLocked(name, time.Now().Add(eventDebounce))
			}
		}
	}
}

// rolloutStatusLocked is the progress of owner's rolling sync, or nil. c.mu
// must be held.
func (c *Controller) rolloutStatusLocked(owner string, now time.Time) *appset.Rollout {
	r := c.rolloutOfLocked(owner)
	if r == nil {
		return nil
	}
	out := &appset.Rollout{Steps: make([]appset.RolloutStep, len(r.steps)), Unselected: r.unselected}
	for i, step := range r.steps {
		out.Steps[i] = appset.RolloutStep{MaxUpdate: step.max, Applications: []appset.RolloutApp{}}
		for _, name := range step.apps {
			state, msg, _ := c.memberLocked(r, name, now)
			if s := c.apps[name]; state == RolloutPending && s != nil && strings.HasPrefix(s.status.Blocked, rolloutPrefix) {
				msg = strings.TrimPrefix(s.status.Blocked, rolloutPrefix)
			}
			out.Steps[i].Applications = append(out.Steps[i].Applications, appset.RolloutApp{Name: name, State: state, Message: msg})
		}
	}
	return out
}

// releaseRevision is the revision of st's latest release, or 0.
func releaseRevision(st *AppStatus) int {
	if st.Release == nil {
		return 0
	}
	return st.Release.Revision
}
