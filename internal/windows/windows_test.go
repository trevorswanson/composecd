package windows

import (
	"strings"
	"testing"
	"time"

	"github.com/trevorswanson/composecd/internal/config"
)

func window(name, kind, schedule string, d time.Duration, manual bool, apps ...string) *config.SyncWindow {
	return &config.SyncWindow{Metadata: config.Metadata{Name: name}, Spec: config.SyncWindowSpec{
		Kind: kind, Schedule: schedule, Duration: config.Duration(d), TimeZone: "America/Chicago",
		Applications: apps, ManualSync: manual,
	}}
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("America/Chicago")
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpen(t *testing.T) {
	w := Compile([]*config.SyncWindow{window("nightly", "allow", "0 2 * * *", 3*time.Hour, false, "*")})[0]
	for now, want := range map[string]bool{
		"2026-10-28 01:59": false, "2026-10-28 02:00": true, "2026-10-28 04:59": true, "2026-10-28 05:00": false, "2026-10-28 12:00": false,
	} {
		open, end := w.Open(at(t, now))
		if open != want {
			t.Errorf("%s: open = %v", now, open)
		}
		if open && !end.Equal(at(t, "2026-10-28 05:00")) {
			t.Errorf("%s: closes %v", now, end)
		}
	}
	if next := w.NextOpen(at(t, "2026-10-28 12:00")); !next.Equal(at(t, "2026-10-29 02:00")) {
		t.Errorf("next open = %v", next)
	}
	// Daylight saving ends on 2026-11-01 in Chicago: 02:00 local still opens the window.
	if open, _ := w.Open(at(t, "2026-11-01 03:00")); !open {
		t.Error("window not open across the DST change")
	}

	// A duration longer than the period stays open, and ends after the last opening.
	busy := Compile([]*config.SyncWindow{window("busy", "deny", "*/10 * * * *", time.Hour, false, "*")})[0]
	open, end := busy.Open(at(t, "2026-10-28 12:05"))
	if !open || !end.Equal(at(t, "2026-10-28 13:00")) {
		t.Errorf("overlapping window: %v until %v", open, end)
	}
}

func TestMatches(t *testing.T) {
	w := Compile([]*config.SyncWindow{{Metadata: config.Metadata{Name: "w"}, Spec: config.SyncWindowSpec{
		Kind: "deny", Schedule: "@daily", Duration: config.Duration(time.Hour),
		Applications: []string{"media-*", "plex"}, Hosts: []string{"nas"}, Labels: map[string]string{"tier": "apps"},
	}}})[0]
	apps := map[string]bool{
		"media-sonarr nas tier=apps": true, "plex nas tier=apps": true, "media-sonarr pi tier=apps": false,
		"media-sonarr nas tier=infra": false, "traefik nas tier=apps": false, "plex nas": false,
	}
	for key, want := range apps {
		f := strings.Fields(key)
		labels := map[string]string{}
		if len(f) > 2 {
			k, v, _ := strings.Cut(f[2], "=")
			labels[k] = v
		}
		if got := w.Matches(f[0], f[1], labels); got != want {
			t.Errorf("%s: %v", key, got)
		}
	}
}

func TestEvaluate(t *testing.T) {
	ws := Compile([]*config.SyncWindow{
		window("nightly", "allow", "0 2 * * *", 3*time.Hour, true, "media"),
		window("freeze", "deny", "0 18 * * 5", 48*time.Hour, false, "*"),
		window("movie-night", "deny", "0 19 * * *", 4*time.Hour, true, "plex"),
	})
	cases := []struct {
		app, now string
		manual   bool
		allowed  bool
		reason   string
	}{
		{"traefik", "2026-10-28 12:00", false, true, ""},
		{"media", "2026-10-28 03:00", false, true, ""},
		{"media", "2026-10-28 12:00", false, false, "outside allow windows; nightly opens Thu 02:00 CDT"},
		{"media", "2026-10-28 12:00", true, true, "manual sync allowed outside allow windows"},
		// Friday 18:00 to Sunday 18:00: deny wins over the open allow window.
		{"media", "2026-10-31 03:00", false, false, "deny window freeze (until Sun 17:00 CST) is open"}, // 48h crosses the DST change
		{"media", "2026-10-31 03:00", true, false, "deny window freeze"},
		{"plex", "2026-10-28 20:00", false, false, "deny window movie-night (until Wed 23:00 CDT) is open"},
		{"plex", "2026-10-28 20:00", true, true, "manual sync allowed by deny window movie-night"},
	}
	for _, tc := range cases {
		d := Evaluate(ws, tc.app, "nas", nil, at(t, tc.now), tc.manual)
		if d.Allowed != tc.allowed || !strings.HasPrefix(d.Reason, tc.reason) || (tc.reason == "" && d.Reason != "") {
			t.Errorf("%s at %s (manual %v): %+v, want %v %q", tc.app, tc.now, tc.manual, d, tc.allowed, tc.reason)
		}
	}

	apps := []*config.Application{
		{Metadata: config.Metadata{Name: "media"}, Spec: config.AppSpec{Destination: config.Destination{Host: "nas"}}},
		{Metadata: config.Metadata{Name: "plex"}, Spec: config.AppSpec{Destination: config.Destination{Host: "nas"}}},
	}
	st := Statuses(ws, apps, at(t, "2026-10-28 20:00"))
	if len(st) != 3 || st[2].Name != "movie-night" || !st[2].Open || strings.Join(st[1].Applications, ",") != "media,plex" || st[0].Open {
		t.Errorf("statuses = %+v", st)
	}
}

func apps(names ...string) []*config.Application {
	var out []*config.Application
	for _, n := range names {
		out = append(out, &config.Application{Metadata: config.Metadata{Name: n}, Spec: config.AppSpec{Destination: config.Destination{Host: "nas"}}})
	}
	return out
}

func names(ws []*Window) string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Name())
	}
	return strings.Join(out, ",")
}

func TestBuiltinDefault(t *testing.T) {
	// Git defines no default, so the built-in one is compiled last.
	ws := Compile([]*config.SyncWindow{window("movie-night", "deny", "0 19 * * *", 4*time.Hour, false, "plex")})
	if len(ws) != 2 || !ws[1].Default() || !ws[1].Builtin() || ws[0].Default() {
		t.Fatalf("compiled = %s", names(ws))
	}
	if err := (&config.Objects{SyncWindows: []*config.SyncWindow{Builtin()}}).Validate(); err != nil {
		t.Errorf("the built-in window is not a valid SyncWindow: %v", err)
	}
	if got := names(Applying(ws, "traefik", "nas", nil)); got != "default" {
		t.Errorf("traefik belongs to %q, want default", got)
	}
	if got := names(Applying(ws, "plex", "nas", nil)); got != "movie-night" {
		t.Errorf("plex belongs to %q", got)
	}
	// Always open, at any time, so unmatched apps sync as they did before.
	for _, now := range []string{"2026-10-28 00:00", "2026-10-28 20:00", "2026-11-01 01:30", "2026-12-31 23:59"} {
		if open, end := ws[1].Open(at(t, now)); !open || !end.IsZero() {
			t.Errorf("built-in default at %s: open %v until %v", now, open, end)
		}
		if d := Evaluate(ws, "traefik", "nas", nil, at(t, now), false); !d.Allowed || d.Reason != "" || strings.Join(d.Windows, ",") != "default" {
			t.Errorf("traefik at %s: %+v", now, d)
		}
	}
	if !ws[1].NextOpen(at(t, "2026-10-28 00:00")).IsZero() {
		t.Error("the built-in default has a next opening")
	}
	if d := Evaluate(ws, "plex", "nas", nil, at(t, "2026-10-28 20:00"), false); d.Allowed || strings.Join(d.Windows, ",") != "movie-night" {
		t.Errorf("plex in movie night: %+v", d)
	}
	// Windows that weren't compiled (no config yet) still allow everything.
	if d := Evaluate(nil, "traefik", "nas", nil, at(t, "2026-10-28 00:00"), false); !d.Allowed || strings.Join(d.Windows, ",") != "default" {
		t.Errorf("no windows: %+v", d)
	}
	refs := Refs(ws, "traefik", "nas", nil, at(t, "2026-10-28 00:00"))
	if len(refs) != 1 || refs[0] != (Ref{Name: "default", Kind: "allow", Open: true, Builtin: true}) {
		t.Errorf("refs = %+v", refs)
	}
	// The default's own (rejected) selectors never match an app directly.
	if ws[1].Matches("traefik", "nas", nil) {
		t.Error("the default window matched by selector")
	}
}

func TestDefaultOverride(t *testing.T) {
	// A deny default from Git: 22:00 to 06:00 every day.
	def := &config.SyncWindow{Metadata: config.Metadata{Name: "default"}, Origin: "windows.yaml:1", Spec: config.SyncWindowSpec{
		Kind: "deny", Schedule: "0 22 * * *", Duration: config.Duration(8 * time.Hour), TimeZone: "America/Chicago", ManualSync: true,
	}}
	ws := Compile([]*config.SyncWindow{def, window("nightly", "allow", "0 2 * * *", 3*time.Hour, false, "media")})
	if names(ws) != "nightly,default" || ws[1].Builtin() || ws[1].Spec != def {
		t.Fatalf("compiled = %s", names(ws))
	}
	cases := []struct {
		app, now string
		manual   bool
		allowed  bool
		reason   string
	}{
		// Unmatched apps follow the default.
		{"traefik", "2026-10-28 12:00", false, true, ""},
		{"traefik", "2026-10-28 23:00", false, false, "deny window default (until Thu 06:00 CDT) is open"},
		{"traefik", "2026-10-28 23:00", true, true, "manual sync allowed by deny window default"},
		// media is matched by nightly, so the default doesn't apply to it.
		{"media", "2026-10-28 03:00", false, true, ""},
		{"media", "2026-10-28 23:00", false, false, "outside allow windows; nightly opens Thu 02:00 CDT"},
		{"media", "2026-10-29 03:00", false, true, ""},
	}
	for _, tc := range cases {
		d := Evaluate(ws, tc.app, "nas", nil, at(t, tc.now), tc.manual)
		if d.Allowed != tc.allowed || !strings.HasPrefix(d.Reason, tc.reason) || (tc.reason == "" && d.Reason != "") {
			t.Errorf("%s at %s (manual %v): %+v, want %v %q", tc.app, tc.now, tc.manual, d, tc.allowed, tc.reason)
		}
	}
	if d := Evaluate(ws, "media", "nas", nil, at(t, "2026-10-28 03:00"), false); strings.Join(d.Windows, ",") != "nightly" {
		t.Errorf("media's windows = %v", d.Windows)
	}

	// An allow default holds unmatched apps outside it.
	allow := Compile([]*config.SyncWindow{{Metadata: config.Metadata{Name: "default"}, Spec: config.SyncWindowSpec{
		Kind: "allow", Schedule: "0 2 * * *", Duration: config.Duration(time.Hour), TimeZone: "America/Chicago",
	}}})
	if d := Evaluate(allow, "traefik", "nas", nil, at(t, "2026-10-28 12:00"), false); d.Allowed || d.Reason != "outside allow windows; default opens Thu 02:00 CDT" {
		t.Errorf("allow default: %+v", d)
	}
}

func TestStatusesDefault(t *testing.T) {
	now := at(t, "2026-10-28 12:00")
	plex := window("movie-night", "deny", "0 19 * * *", 4*time.Hour, false, "plex")
	all := window("freeze", "deny", "0 18 * * 5", 48*time.Hour, false, "*")

	// Some apps are unmatched: the built-in default lists them.
	st := Statuses(Compile([]*config.SyncWindow{plex}), apps("traefik", "plex", "media"), now)
	if len(st) != 2 || st[0].Name != "movie-night" || st[0].Default || st[0].Builtin {
		t.Fatalf("statuses = %+v", st)
	}
	d := st[1]
	if d.Name != "default" || !d.Default || !d.Builtin || !d.Open || d.Kind != "allow" || d.Origin != "" ||
		!d.ClosesAt.IsZero() || !d.NextOpen.IsZero() || strings.Join(d.Applications, ",") != "media,traefik" {
		t.Errorf("built-in default = %+v", d)
	}

	// Every app is matched, and Git defines no default: it isn't shown.
	st = Statuses(Compile([]*config.SyncWindow{plex, all}), apps("traefik", "plex"), now)
	if len(st) != 2 || st[0].Name != "movie-night" || st[1].Name != "freeze" {
		t.Errorf("statuses without unmatched apps = %+v", st)
	}
	if st := Statuses(Compile(nil), nil, now); len(st) != 0 {
		t.Errorf("statuses without apps or windows = %+v", st)
	}

	// A default from Git is shown even when every app is matched.
	def := &config.SyncWindow{Metadata: config.Metadata{Name: "default"}, Origin: "windows.yaml:9", Spec: config.SyncWindowSpec{
		Kind: "deny", Schedule: "0 22 * * *", Duration: config.Duration(8 * time.Hour), TimeZone: "America/Chicago",
	}}
	st = Statuses(Compile([]*config.SyncWindow{def, all}), apps("traefik", "plex"), now)
	if len(st) != 2 || st[1].Name != "default" || !st[1].Default || st[1].Builtin || st[1].Origin != "windows.yaml:9" ||
		st[1].Open || st[1].NextOpen.IsZero() || len(st[1].Applications) != 0 || st[1].Applications == nil {
		t.Errorf("git default = %+v", st)
	}
}
