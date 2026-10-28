package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/trevorswanson/composetpl/pkg/action"
	"github.com/trevorswanson/composetpl/pkg/release"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/store"
	"github.com/trevorswanson/composecd/internal/windows"
)

// prune uninstalls releases composecd deployed whose Application is no longer
// in Git for their host (removed, or moved to another host), if the app had
// prune on when it was last synced. Others are recorded as orphans. fresh is
// false when the objects couldn't be read from Git this refresh; then nothing
// is pruned.
func (c *Controller) prune(ctx context.Context, objs *config.Objects, fresh bool) {
	orphans := map[string][]string{}
	for _, h := range c.Hosts.All() {
		if h.Status().State != hosts.StateUp {
			c.mu.RLock()
			orphans[h.Name()] = c.orphans[h.Name()] // keep what we knew
			c.mu.RUnlock()
			continue
		}
		for _, rel := range c.leftovers(h, objs) {
			name := rel.Name
			reason := ""
			hs, _ := c.Store.HostState(h.Name())
			c.mu.RLock()
			ws := c.windows
			c.mu.RUnlock()
			switch {
			case rel.Labels[LabelPrune] != "true":
				reason = "prune is off"
			case !fresh:
				reason = "the root could not be read from Git"
			case len(objs.Applications) == 0:
				reason = "the root defines no applications, which looks like a mistake"
			case hs.Maintenance:
				reason = "the host is in maintenance"
			default:
				if d := windows.Evaluate(ws, name, h.Name(), nil, time.Now(), false); !d.Allowed {
					reason = d.Reason
				}
			}
			if reason != "" {
				c.Log.Debug("not pruning", "app", name, "host", h.Name(), "reason", reason)
				orphans[h.Name()] = append(orphans[h.Name()], name)
				continue
			}
			if !c.pruneOne(ctx, h, rel) {
				orphans[h.Name()] = append(orphans[h.Name()], name)
			}
		}
		sort.Strings(orphans[h.Name()])
	}
	c.mu.Lock()
	c.orphans = orphans
	c.mu.Unlock()
}

// leftovers are h's composecd releases with no Application for h in objs.
func (c *Controller) leftovers(h *hosts.Host, objs *config.Objects) []*release.Release {
	defer h.Use()()
	rels, err := h.Store().List()
	if err != nil {
		c.Log.Warn("listing releases for prune", "host", h.Name(), "error", err)
		return nil
	}
	var out []*release.Release
	for _, r := range rels {
		if r.Labels[LabelManagedBy] != ManagedBy || r.Status == release.StatusUninstalled {
			continue
		}
		if a := objs.Application(r.Name); a != nil && a.Spec.Destination.Host == h.Name() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// pruneOne uninstalls rel from h, keeping its history and named volumes.
func (c *Controller) pruneOne(ctx context.Context, h *hosts.Host, rel *release.Release) bool {
	name := rel.Name
	op, err := c.startOperation(name, store.KindPrune, store.TriggerAuto, rel.Labels[LabelSourceRevision], "")
	if err != nil {
		return false
	}
	defer c.lockApp(name)()
	c.Log.Info("pruning", "app", name, "host", h.Name())
	func() {
		defer h.Use()()
		log := &logBuffer{max: maxLog}
		cfg := h.Configuration(nil, log)
		u := action.NewUninstall(cfg)
		u.KeepHistory, u.Timeout = true, config.DefaultTimeout
		_, err := u.Run(ctx, name)
		op.Log = log.String()
		if err != nil {
			op.Phase, op.Message = store.PhaseFailed, err.Error()
			return
		}
		op.Phase = store.PhaseSucceeded
		op.Message = fmt.Sprintf("uninstalled from %s: the Application is no longer in Git for this host (history and volumes kept)", h.Name())
	}()
	op.ReleaseRevision = rel.Revision
	c.finishOperation(name, op)
	return op.Phase == store.PhaseSucceeded
}

// Operation metrics, by kind, trigger and phase.
type metricKey struct{ Kind, Trigger, Phase string }

type opMetric struct {
	Count   int
	Seconds float64
}

// OperationMetric is one row of OperationMetrics.
type OperationMetric struct {
	Kind, Trigger, Phase string
	Count                int
	Seconds              float64
}

// OperationMetrics counts the operations finished since start, with their
// total duration.
func (c *Controller) OperationMetrics() []OperationMetric {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]OperationMetric, 0, len(c.metrics))
	for k, m := range c.metrics {
		out = append(out, OperationMetric{Kind: k.Kind, Trigger: k.Trigger, Phase: k.Phase, Count: m.Count, Seconds: m.Seconds})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Trigger != b.Trigger {
			return a.Trigger < b.Trigger
		}
		return a.Phase < b.Phase
	})
	return out
}
