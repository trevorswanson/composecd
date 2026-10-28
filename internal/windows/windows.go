// Package windows evaluates SyncWindows with Argo CD's semantics:
//
//   - an open deny window that matches an app blocks its syncs;
//   - if any allow windows match an app, it may only sync while one is open;
//   - a blocking window with manualSync lets manual syncs through.
//
// Automatic syncs, self-heal and prune are all subject to windows.
package windows

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trevorswanson/composecd/internal/config"
)

// Window is a compiled SyncWindow.
type Window struct {
	Spec  *config.SyncWindow
	sched cron.Schedule
	loc   *time.Location
}

// Compile prepares validated windows; invalid ones (which validation rejects)
// are skipped.
func Compile(ws []*config.SyncWindow) []*Window {
	var out []*Window
	for _, w := range ws {
		sched, err := config.ParseSchedule(w.Spec.Schedule)
		if err != nil {
			continue
		}
		loc, err := time.LoadLocation(w.Spec.TimeZone)
		if err != nil {
			continue
		}
		out = append(out, &Window{Spec: w, sched: sched, loc: loc})
	}
	return out
}

// Name is the window's name.
func (w *Window) Name() string { return w.Spec.Metadata.Name }

func (w *Window) duration() time.Duration { return time.Duration(w.Spec.Spec.Duration) }

// Open reports whether the window is open at now and, if so, when it closes.
func (w *Window) Open(now time.Time) (bool, time.Time) {
	// The newest opening at or before now that is still within Duration.
	start := w.sched.Next(now.Add(-w.duration()).In(w.loc))
	if start.After(now) {
		return false, time.Time{}
	}
	// Later openings up to now (when Duration exceeds the schedule's period)
	// push the close back; bounded for schedules like every minute.
	end := start.Add(w.duration())
	for next, i := w.sched.Next(start), 0; !next.After(now) && i < 10000; next, i = w.sched.Next(next), i+1 {
		end = next.Add(w.duration())
	}
	return true, end
}

// NextOpen is when the window next opens after now.
func (w *Window) NextOpen(now time.Time) time.Time { return w.sched.Next(now.In(w.loc)) }

// Matches reports whether the window applies to an app on host with labels.
func (w *Window) Matches(app, host string, labels map[string]string) bool {
	s := w.Spec.Spec
	if len(s.Applications) > 0 && !anyMatch(s.Applications, app) {
		return false
	}
	if len(s.Hosts) > 0 && !anyMatch(s.Hosts, host) {
		return false
	}
	for k, v := range s.Labels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func anyMatch(globs []string, s string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, s); ok {
			return true
		}
	}
	return false
}

// Decision is whether a sync may run now.
type Decision struct {
	Allowed bool `json:"allowed"`
	// Reason explains a block, or a manual sync let through.
	Reason string `json:"reason,omitempty"`
	// Windows are the names of the windows that match the app.
	Windows []string `json:"windows,omitempty"`
}

// Evaluate decides whether app (on host, with labels) may sync at now.
func Evaluate(ws []*Window, app, host string, labels map[string]string, now time.Time, manual bool) Decision {
	var matching []string
	var openDeny, allows, openAllow []*Window
	ends := map[*Window]time.Time{}
	for _, w := range ws {
		if !w.Matches(app, host, labels) {
			continue
		}
		matching = append(matching, w.Name())
		open, end := w.Open(now)
		ends[w] = end
		switch w.Spec.Spec.Kind {
		case config.WindowDeny:
			if open {
				openDeny = append(openDeny, w)
			}
		case config.WindowAllow:
			allows = append(allows, w)
			if open {
				openAllow = append(openAllow, w)
			}
		}
	}
	d := Decision{Allowed: true, Windows: matching}
	switch {
	case len(openDeny) > 0:
		var parts []string
		for _, w := range openDeny {
			parts = append(parts, fmt.Sprintf("%s (until %s)", w.Name(), clock(ends[w], w.loc)))
		}
		if manual && allManual(openDeny) {
			d.Reason = "manual sync allowed by deny window " + strings.Join(parts, ", ")
			return d
		}
		d.Allowed, d.Reason = false, "deny window "+strings.Join(parts, ", ")+" is open"
	case len(allows) > 0 && len(openAllow) == 0:
		next := allows[0]
		for _, w := range allows[1:] {
			if w.NextOpen(now).Before(next.NextOpen(now)) {
				next = w
			}
		}
		if manual && allManual(allows) {
			d.Reason = "manual sync allowed outside allow windows"
			return d
		}
		d.Allowed = false
		d.Reason = fmt.Sprintf("outside allow windows; %s opens %s", next.Name(), clock(next.NextOpen(now), next.loc))
	}
	return d
}

func allManual(ws []*Window) bool {
	for _, w := range ws {
		if !w.Spec.Spec.ManualSync {
			return false
		}
	}
	return true
}

func clock(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon 15:04 MST")
}

// Status is a window as the API shows it.
type Status struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Schedule   string    `json:"schedule"`
	Duration   string    `json:"duration"`
	TimeZone   string    `json:"timeZone,omitempty"`
	ManualSync bool      `json:"manualSync,omitempty"`
	Open       bool      `json:"open"`
	ClosesAt   time.Time `json:"closesAt,omitempty"`
	NextOpen   time.Time `json:"nextOpen"`
	// Applications are the apps the window matches now.
	Applications []string `json:"applications"`
}

// Statuses describes every window at now, with the apps (name, host, labels)
// each one matches.
func Statuses(ws []*Window, apps []*config.Application, now time.Time) []Status {
	out := []Status{}
	for _, w := range ws {
		open, end := w.Open(now)
		s := Status{
			Name: w.Name(), Kind: w.Spec.Spec.Kind, Schedule: w.Spec.Spec.Schedule, Duration: w.Spec.Spec.Duration.String(),
			TimeZone: w.Spec.Spec.TimeZone, ManualSync: w.Spec.Spec.ManualSync, Open: open, ClosesAt: end,
			NextOpen: w.NextOpen(now), Applications: []string{},
		}
		for _, a := range apps {
			if w.Matches(a.Metadata.Name, a.Spec.Destination.Host, a.Metadata.Labels) {
				s.Applications = append(s.Applications, a.Metadata.Name)
			}
		}
		sort.Strings(s.Applications)
		out = append(out, s)
	}
	return out
}
