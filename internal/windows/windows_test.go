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
