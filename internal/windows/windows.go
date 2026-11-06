// Package windows evaluates SyncWindows with Argo CD's semantics:
//
//   - an open deny window that matches an app blocks its syncs;
//   - if any allow windows match an app, it may only sync while one is open;
//   - a blocking window with manualSync lets manual syncs through.
//
// An app that no other window matches belongs to the window named default
// (config.DefaultWindow). A SyncWindow named default in Git sets its schedule;
// without one, a built-in default that is always open applies, so such apps
// may sync at any time.
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
	// builtin is the default window composecd adds when Git defines none.
	builtin bool
}

// Builtin is the default window when Git doesn't define one: an allow window
// that opens every minute for a minute, so it is always open.
func Builtin() *config.SyncWindow {
	return &config.SyncWindow{Metadata: config.Metadata{Name: config.DefaultWindow}, Spec: config.SyncWindowSpec{
		Kind: config.WindowAllow, Schedule: "* * * * *", Duration: config.Duration(time.Minute), TimeZone: "UTC",
	}}
}

// Compile prepares validated windows; invalid ones (which validation rejects)
// are skipped. The default window comes last: the one in ws, or Builtin.
func Compile(ws []*config.SyncWindow) []*Window {
	var out []*Window
	var def *Window
	for _, w := range ws {
		c := compile(w)
		switch {
		case c == nil:
		case c.Default():
			def = c
		default:
			out = append(out, c)
		}
	}
	if def == nil {
		def = builtin
	}
	return append(out, def)
}

// builtin is the compiled Builtin window.
var builtin = func() *Window {
	w := compile(Builtin())
	w.builtin = true
	return w
}()

func compile(w *config.SyncWindow) *Window {
	sched, err := config.ParseSchedule(w.Spec.Schedule)
	if err != nil {
		return nil
	}
	loc, err := time.LoadLocation(w.Spec.TimeZone)
	if err != nil {
		return nil
	}
	return &Window{Spec: w, sched: sched, loc: loc}
}

// Name is the window's name.
func (w *Window) Name() string { return w.Spec.Metadata.Name }

// Default reports whether w is the default window, from Git or built in.
func (w *Window) Default() bool { return w.Name() == config.DefaultWindow }

// Builtin reports whether w is the built-in default window.
func (w *Window) Builtin() bool { return w.builtin }

func (w *Window) duration() time.Duration { return time.Duration(w.Spec.Spec.Duration) }

// Open reports whether the window is open at now and, if so, when it closes.
// The built-in default is always open, and never closes (zero time).
func (w *Window) Open(now time.Time) (bool, time.Time) {
	if w.builtin {
		return true, time.Time{}
	}
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

// NextOpen is when the window next opens after now (zero for the built-in
// default, which never closes).
func (w *Window) NextOpen(now time.Time) time.Time {
	if w.builtin {
		return time.Time{}
	}
	return w.sched.Next(now.In(w.loc))
}

// Matches reports whether the window's selectors match an app on host with
// labels. The default window has none and matches nothing by itself: see
// Applying for the apps it applies to.
func (w *Window) Matches(app, host string, labels map[string]string) bool {
	if w.Default() {
		return false
	}
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

// Applying is the windows that apply to app (on host, with labels): those
// whose selectors match it or, when none do, the default window. ws normally
// comes from Compile; without a default in it, the built-in one applies.
func Applying(ws []*Window, app, host string, labels map[string]string) []*Window {
	var out []*Window
	var def *Window
	for _, w := range ws {
		if w.Default() {
			def = w
		} else if w.Matches(app, host, labels) {
			out = append(out, w)
		}
	}
	if len(out) > 0 {
		return out
	}
	if def == nil {
		def = builtin
	}
	return []*Window{def}
}

// Ref is a window that applies to an app, as the app's status shows it.
type Ref struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Open bool   `json:"open"`
	// Builtin is true for the built-in default window.
	Builtin bool `json:"builtin,omitempty"`
}

// Refs describes the windows that apply to app (on host, with labels) at now.
func Refs(ws []*Window, app, host string, labels map[string]string, now time.Time) []Ref {
	var out []Ref
	for _, w := range Applying(ws, app, host, labels) {
		open, _ := w.Open(now)
		out = append(out, Ref{Name: w.Name(), Kind: w.Spec.Spec.Kind, Open: open, Builtin: w.builtin})
	}
	return out
}

// Evaluate decides whether app (on host, with labels) may sync at now.
func Evaluate(ws []*Window, app, host string, labels map[string]string, now time.Time, manual bool) Decision {
	var matching []string
	var openDeny, allows, openAllow []*Window
	ends := map[*Window]time.Time{}
	for _, w := range Applying(ws, app, host, labels) {
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
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Schedule   string `json:"schedule"`
	Duration   string `json:"duration"`
	TimeZone   string `json:"timeZone,omitempty"`
	ManualSync bool   `json:"manualSync,omitempty"`
	// Default is true for the window of the apps no other window matches.
	Default bool `json:"default,omitempty"`
	// Builtin is true for the default window composecd adds when Git doesn't
	// define one. It is always open, so its apps may sync at any time.
	Builtin bool `json:"builtin,omitempty"`
	// Origin is where Git defines the window (file:line).
	Origin string `json:"origin,omitempty"`
	Open   bool   `json:"open"`
	// ClosesAt is when an open window closes; NextOpen is when it next opens.
	// Both are unset for the built-in default, which never closes.
	ClosesAt time.Time `json:"closesAt,omitzero"`
	NextOpen time.Time `json:"nextOpen,omitzero"`
	// Applications are the apps the window applies to now.
	Applications []string `json:"applications"`
}

// Statuses describes every window at now, with the apps (name, host, labels)
// each one applies to. The built-in default is left out when it applies to
// no app; a default defined in Git is always shown.
func Statuses(ws []*Window, apps []*config.Application, now time.Time) []Status {
	out := []Status{}
	members := map[*Window][]string{}
	for _, a := range apps {
		for _, w := range Applying(ws, a.Metadata.Name, a.Spec.Destination.Host, a.Metadata.Labels) {
			members[w] = append(members[w], a.Metadata.Name)
		}
	}
	for _, w := range ws {
		if w.builtin && len(members[w]) == 0 {
			continue
		}
		open, end := w.Open(now)
		s := Status{
			Name: w.Name(), Kind: w.Spec.Spec.Kind, Schedule: w.Spec.Spec.Schedule, Duration: w.Spec.Spec.Duration.String(),
			TimeZone: w.Spec.Spec.TimeZone, ManualSync: w.Spec.Spec.ManualSync, Default: w.Default(), Builtin: w.builtin,
			Origin: w.Spec.Origin, Open: open, ClosesAt: end, NextOpen: w.NextOpen(now), Applications: []string{},
		}
		s.Applications = append(s.Applications, members[w]...)
		sort.Strings(s.Applications)
		out = append(out, s)
	}
	return out
}
