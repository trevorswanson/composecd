package api

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/trevorswanson/composecd/internal/hosts"
)

// metrics serves Prometheus' text exposition format. It needs no token: it
// holds names and states, not secrets.
func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m := &metricWriter{w: w}

	apps := s.c.Apps()
	m.help("composecd_app_sync_status", "gauge", "1 for the application's current sync status (Synced, OutOfSync, Unknown).")
	for _, a := range apps {
		m.sample("composecd_app_sync_status", 1, "app", a.Name, "host", a.Host, "status", string(a.Sync.Status))
	}
	m.help("composecd_app_health_status", "gauge", "1 for the application's current health status.")
	for _, a := range apps {
		m.sample("composecd_app_health_status", 1, "app", a.Name, "host", a.Host, "status", string(a.Health.Status))
	}
	m.help("composecd_app_paused", "gauge", "1 if automatic sync of the application is paused.")
	for _, a := range apps {
		m.sample("composecd_app_paused", b2f(a.Paused), "app", a.Name)
	}
	m.help("composecd_app_blocked", "gauge", "1 if automation is holding off (window, dependency, maintenance, backoff).")
	for _, a := range apps {
		m.sample("composecd_app_blocked", b2f(a.Blocked != ""), "app", a.Name)
	}

	hs := s.c.HostStatuses()
	m.help("composecd_host_up", "gauge", "1 if the host answered its last ping.")
	for _, h := range hs {
		m.sample("composecd_host_up", b2f(h.State == hosts.StateUp), "host", h.Name)
	}
	m.help("composecd_host_maintenance", "gauge", "1 if the host is in maintenance mode.")
	for _, h := range hs {
		m.sample("composecd_host_maintenance", b2f(h.Maintenance), "host", h.Name)
	}
	m.help("composecd_host_orphaned_releases", "gauge", "Releases left on the host whose Application is gone and that weren't pruned.")
	for _, h := range hs {
		m.sample("composecd_host_orphaned_releases", float64(len(h.Orphans)), "host", h.Name)
	}

	ops := s.c.OperationMetrics()
	m.help("composecd_operations_total", "counter", "Operations finished since composecd started.")
	for _, o := range ops {
		m.sample("composecd_operations_total", float64(o.Count), "kind", o.Kind, "trigger", o.Trigger, "phase", o.Phase)
	}
	m.help("composecd_operation_duration_seconds_total", "counter", "Total time spent in finished operations.")
	for _, o := range ops {
		m.sample("composecd_operation_duration_seconds_total", o.Seconds, "kind", o.Kind, "trigger", o.Trigger, "phase", o.Phase)
	}

	root := s.c.Root()
	m.help("composecd_root_last_refresh_timestamp_seconds", "gauge", "When the Git root was last refreshed.")
	ts := 0.0
	if !root.RefreshedAt.IsZero() {
		ts = float64(root.RefreshedAt.UnixMilli()) / 1000
	}
	m.sample("composecd_root_last_refresh_timestamp_seconds", ts)
	m.help("composecd_root_error", "gauge", "1 if the last refresh of the Git root failed.")
	m.sample("composecd_root_error", b2f(root.Error != ""))

	ws := s.c.Windows()
	sort.Slice(ws, func(i, j int) bool { return ws[i].Name < ws[j].Name })
	m.help("composecd_sync_window_open", "gauge", "1 while the sync window is open.")
	for _, win := range ws {
		m.sample("composecd_sync_window_open", b2f(win.Open), "window", win.Name, "kind", win.Kind)
	}
}

type metricWriter struct{ w io.Writer }

func (m *metricWriter) help(name, kind, help string) {
	fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func (m *metricWriter) sample(name string, v float64, labels ...string) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s=%q", labels[i], labels[i+1])
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(m.w, "%s %g\n", b.String(), v)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
