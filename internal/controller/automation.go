package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/health"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

// Self-heal backs off between consecutive heals of an app that keeps drifting,
// so composecd doesn't fight another tool (or a crash loop) in a tight loop.
var (
	healBackoffBase = 5 * time.Second
	healBackoffMax  = 5 * time.Minute
)

func healBackoff(n int) time.Duration {
	return config.Backoff{Duration: config.Duration(healBackoffBase), Factor: 2,
		MaxDuration: config.Duration(healBackoffMax)}.Delay(n)
}

// automate decides what automation should do for app given its assessment, and
// annotates st with why it is holding off. With act, it also runs the sync or
// self-heal and reports true; without, it only annotates.
func (c *Controller) automate(ctx context.Context, app *config.Application, st *AppStatus, p *plan, act bool) bool {
	name := app.Metadata.Name
	auto := app.Spec.SyncPolicy.Automated
	retry := app.Spec.SyncPolicy.Retry
	if auto == nil || st.Paused || p == nil {
		return false
	}
	var kind string
	switch {
	case p.gitChanged:
		kind = store.KindSync
	case auto.SelfHeal && p.drifted:
		kind = store.KindHeal
	default:
		if act {
			c.mu.Lock()
			if s := c.apps[name]; s != nil {
				s.heals, s.nextHeal = 0, time.Time{}
			}
			c.mu.Unlock()
		}
		return false
	}
	if reason := c.gate(app, kind); reason != "" {
		st.Blocked = reason
		return false
	}

	now := time.Now()
	c.mu.Lock()
	s := c.apps[name]
	if s == nil {
		s = &appState{}
		c.apps[name] = s
	}
	if c.running[name] != 0 {
		c.mu.Unlock()
		return false
	}
	var rolling bool
	if kind == store.KindSync {
		var reason string
		var wake time.Time
		reason, wake, rolling = c.rolloutBlockLocked(app, st.generation, now)
		if !wake.IsZero() {
			c.scheduleLocked(name, wake)
		}
		if reason != "" {
			st.Blocked = reason
			c.mu.Unlock()
			return false
		}
	}
	switch kind {
	case store.KindSync:
		if s.attempted == p.key {
			st.Retry = retryStatus(retry, s)
			switch {
			case retry == nil || s.failures == 0 || s.failures > retry.Limit:
				st.Blocked = "the last sync of this revision failed; automatic sync waits for a change in Git (or sync manually)"
				c.mu.Unlock()
				return false
			case now.Before(s.nextRetry):
				st.Blocked = fmt.Sprintf("retrying in %s (retry %d of %d)", s.nextRetry.Sub(now).Round(time.Second), s.failures, retry.Limit)
				c.scheduleLocked(name, s.nextRetry)
				c.mu.Unlock()
				return false
			}
		} else if act {
			s.attempted, s.failures, s.nextRetry = p.key, 0, time.Time{}
		}
	case store.KindHeal:
		if now.Before(s.nextHeal) {
			st.Blocked = fmt.Sprintf("self-heal backing off until %s after %d heals in a row", s.nextHeal.Format(time.TimeOnly), s.heals)
			c.scheduleLocked(name, s.nextHeal)
			c.mu.Unlock()
			return false
		}
		if act {
			s.heals++
			s.nextHeal = now.Add(healBackoff(s.heals))
		}
	}
	// Hold the app's place in the rollout until its status shows the sync.
	s.rollingOut = rolling && act
	c.mu.Unlock()
	if !act {
		return false
	}

	c.setStatus(st)
	switch kind {
	case store.KindSync:
		c.Log.Info("auto-syncing", "app", name, "revision", short(p.prep.Revision), "reasons", strings.Join(st.Sync.Reasons, "; "))
		op, err := c.startOperation(name, store.KindSync, store.TriggerAuto, p.prep.Revision, "")
		if err != nil {
			return false
		}
		c.sync(ctx, p, op)
		c.finishOperation(name, op)
		c.mu.Lock()
		if op.Phase == store.PhaseFailed {
			s.failures++
			if retry != nil && s.failures <= retry.Limit {
				s.nextRetry = time.Now().Add(retry.Backoff.Delay(s.failures))
				c.scheduleLocked(name, s.nextRetry)
			}
		} else {
			s.failures = 0
		}
		c.mu.Unlock()
	case store.KindHeal:
		c.Log.Info("self-healing", "app", name, "drift", driftSummary(st.Sync.Drift))
		op, err := c.startOperation(name, store.KindHeal, store.TriggerAuto, st.Sync.DeployedRevision, "")
		if err != nil {
			return false
		}
		c.heal(ctx, p, op, driftSummary(st.Sync.Drift))
		c.finishOperation(name, op)
	}
	return true
}

func retryStatus(r *config.Retry, s *appState) *RetryStatus {
	if r == nil || s.failures == 0 {
		return nil
	}
	rs := &RetryStatus{Failures: s.failures, Limit: r.Limit}
	if s.failures <= r.Limit {
		rs.NextAt = s.nextRetry
	}
	return rs
}

// gate returns why automation may not run kind for app now, or "".
func (c *Controller) gate(app *config.Application, kind string) string {
	if c.isSelfApp(app) && !c.canUpdateSelf() {
		return selfBlocked
	}
	host := app.Spec.Destination.Host
	if hs, err := c.Store.HostState(host); err == nil && hs.Maintenance {
		return fmt.Sprintf("host %s is in maintenance (%s)", host, hs.MaintenanceReason)
	}
	c.mu.RLock()
	ws := c.windows
	c.mu.RUnlock()
	if d := windows.Evaluate(ws, app.Metadata.Name, host, app.Metadata.Labels, time.Now(), false); !d.Allowed {
		return d.Reason
	}
	if kind != store.KindSync {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, dep := range app.Spec.DependsOn {
		s, ok := c.apps[dep]
		if !ok {
			return fmt.Sprintf("waiting for dependency %s (not reconciled yet)", dep)
		}
		if s.status.Sync.Status != Synced || s.status.Health.Status != health.Healthy {
			return fmt.Sprintf("waiting for dependency %s (%s, %s)", dep, s.status.Sync.Status, s.status.Health.Status)
		}
	}
	return ""
}

// scheduleLocked reconciles name at (or soon after) at. c.mu must be held.
func (c *Controller) scheduleLocked(name string, at time.Time) {
	if t, ok := c.timers[name]; ok && !t.when.After(at) {
		return
	} else if ok {
		t.timer.Stop()
	}
	d := time.Until(at) + 100*time.Millisecond
	c.timers[name] = &timer{when: at, timer: time.AfterFunc(d, func() {
		c.mu.Lock()
		delete(c.timers, name)
		ctx := c.ctx
		c.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if app, err := c.application(name); err == nil {
			c.reconcileAsync(app, false)
		}
	})}
}

type timer struct {
	when  time.Time
	timer *time.Timer
}

// runHeal re-applies the deployed revision to drifted containers.
func (c *Controller) runHeal(ctx context.Context, p *plan, op *store.Operation, drift string) {
	defer p.host.Use()()
	log := c.opLog(op)
	cfg := p.host.Configuration(p.prep.Env, log)
	r := action.NewRepair(cfg)
	r.DeployOptions = deployOptions(p.app.Spec.SyncPolicy.Options)
	r.ForceRecreate = false
	rel, err := r.Run(ctx, p.app.Metadata.Name)
	op.Log = log.String()
	if rel != nil {
		op.ReleaseRevision = rel.Revision
	}
	if err != nil {
		op.Phase, op.Message = store.PhaseFailed, err.Error()
		return
	}
	op.Phase, op.Message = store.PhaseSucceeded, fmt.Sprintf("repaired %s to revision %d", drift, rel.Revision)
}

func driftSummary(drift []action.ServiceDrift) string {
	var parts []string
	for _, d := range drift {
		if d.Reason != action.DriftStopped {
			parts = append(parts, d.Service+" "+d.Reason)
		}
	}
	return strings.Join(parts, ", ")
}
