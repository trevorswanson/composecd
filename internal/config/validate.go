package config

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trevorswanson/composetpl/pkg/registry"
	"github.com/trevorswanson/composetpl/pkg/release"
)

// Validate checks every object and the references between them, returning all
// problems at once.
func (o *Objects) Validate() error {
	var errs []error
	fail := func(origin, format string, args ...interface{}) {
		errs = append(errs, fmt.Errorf("%s: %s", origin, fmt.Sprintf(format, args...)))
	}

	hosts := map[string]string{}
	for _, h := range o.Hosts {
		name := h.Metadata.Name
		if err := release.ValidateName(name); err != nil {
			fail(h.Origin, "host: %v", strings.Replace(err.Error(), "release name", "name", 1))
		}
		if prev, ok := hosts[name]; ok {
			fail(h.Origin, "host %q is already defined at %s", name, prev)
		}
		hosts[name] = h.Origin
		if err := release.ValidateLabels(h.Metadata.Labels); err != nil {
			fail(h.Origin, "host %q: %v", name, err)
		}
		s := h.Spec
		if s.URL != "" && s.Context != "" {
			fail(h.Origin, "host %q: set url or context, not both", name)
		}
		if s.URL != "" && !hasScheme(s.URL, "unix://", "ssh://", "tcp://", "npipe://") {
			fail(h.Origin, "host %q: url %q must start with unix://, ssh://, tcp:// or npipe://", name, s.URL)
		}
		if s.TLSCertPath != "" && !strings.HasPrefix(s.URL, "tcp://") {
			fail(h.Origin, "host %q: tlsCertPath only applies to tcp:// urls", name)
		}
		if s.ProjectRoot != "" && !strings.HasPrefix(s.ProjectRoot, "/") && !isWindowsAbs(s.ProjectRoot) {
			fail(h.Origin, "host %q: projectRoot %q must be an absolute path", name, s.ProjectRoot)
		}
		switch s.Storage {
		case "", StorageVolume, StorageLocal:
		default:
			fail(h.Origin, "host %q: storage %q must be %s or %s", name, s.Storage, StorageVolume, StorageLocal)
		}
	}

	apps := map[string]string{}
	for _, a := range o.Applications {
		name := a.Metadata.Name
		if err := release.ValidateName(name); err != nil {
			fail(a.Origin, "application: %v", strings.Replace(err.Error(), "release name", "name", 1))
		}
		if prev, ok := apps[name]; ok {
			fail(a.Origin, "application %q is already defined at %s", name, prev)
		}
		apps[name] = a.Origin
		if err := release.ValidateLabels(a.Metadata.Labels); err != nil {
			fail(a.Origin, "application %q: %v", name, err)
		}
		s := a.Spec.Source
		switch {
		case s.IsOCI():
			if !registry.IsOCI(s.Chart) {
				fail(a.Origin, "application %q: chart %q must be an oci:// reference", name, s.Chart)
			}
			if s.RepoURL != "" || s.Path != "" || s.TargetRevision != "" {
				fail(a.Origin, "application %q: set chart, or repoURL/targetRevision/path, not both", name)
			}
			if len(s.ValueFiles) > 0 && s.ValuesFrom == nil {
				fail(a.Origin, "application %q: valueFiles with an OCI chart require valuesFrom", name)
			}
		default:
			if s.ValuesFrom != nil {
				fail(a.Origin, "application %q: valuesFrom only applies to an OCI chart", name)
			}
			if s.Version != "" {
				fail(a.Origin, "application %q: version only applies to an oci:// chart", name)
			}
			if p, err := cleanRelative(s.Path); err != nil {
				fail(a.Origin, "application %q: path %v", name, err)
			} else {
				a.Spec.Source.Path = p
			}
		}
		base := a.Spec.Source.Path
		if s.IsOCI() {
			base = ""
		}
		for i, vf := range s.ValueFiles {
			joined := path.Clean(path.Join(base, vf))
			if vf == "" || path.IsAbs(vf) || filepath.IsAbs(vf) || filepath.VolumeName(vf) != "" || isWindowsAbs(vf) || strings.Contains(vf, `\`) || joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
				fail(a.Origin, "application %q: valueFiles[%d] %q must be a file inside the source repository", name, i, vf)
			}
		}

		if a.Spec.Destination.Host == "" {
			fail(a.Origin, "application %q: destination.host is required", name)
		} else if _, ok := hosts[a.Spec.Destination.Host]; !ok {
			fail(a.Origin, "application %q: destination host %q is not defined", name, a.Spec.Destination.Host)
		}
		opts := a.Spec.SyncPolicy.Options
		switch opts.Pull {
		case "", "always", "missing", "never":
		default:
			fail(a.Origin, "application %q: syncPolicy.options.pull %q must be always, missing or never", name, opts.Pull)
		}
		if opts.HistoryMax != nil && *opts.HistoryMax < 0 {
			fail(a.Origin, "application %q: syncPolicy.options.historyMax must not be negative", name)
		}
		if opts.Timeout < 0 {
			fail(a.Origin, "application %q: syncPolicy.options.timeout must not be negative", name)
		}
		if r := a.Spec.SyncPolicy.Retry; r != nil {
			if r.Limit < 0 {
				fail(a.Origin, "application %q: syncPolicy.retry.limit must not be negative", name)
			}
			if b := r.Backoff; b.Duration < 0 || b.MaxDuration < 0 || (b.Factor != 0 && b.Factor < 1) {
				fail(a.Origin, "application %q: syncPolicy.retry.backoff needs durations >= 0 and a factor >= 1", name)
			}
		}
		for _, dep := range a.Spec.DependsOn {
			if dep == name {
				fail(a.Origin, "application %q: dependsOn names itself", name)
			}
		}
	}
	for _, a := range o.Applications {
		for _, dep := range a.Spec.DependsOn {
			if _, ok := apps[dep]; !ok {
				fail(a.Origin, "application %q: dependsOn %q is not defined", a.Metadata.Name, dep)
			}
		}
	}
	if cycle := o.dependencyCycle(); cycle != nil {
		fail(o.Application(cycle[0]).Origin, "dependsOn cycle: %s", strings.Join(cycle, " -> "))
	}

	windows := map[string]string{}
	for _, w := range o.SyncWindows {
		name := w.Metadata.Name
		if err := release.ValidateName(name); err != nil {
			fail(w.Origin, "sync window: %v", strings.Replace(err.Error(), "release name", "name", 1))
		}
		if prev, ok := windows[name]; ok {
			fail(w.Origin, "sync window %q is already defined at %s", name, prev)
		}
		windows[name] = w.Origin
		s := w.Spec
		if s.Kind != WindowAllow && s.Kind != WindowDeny {
			fail(w.Origin, "sync window %q: kind %q must be %s or %s", name, s.Kind, WindowAllow, WindowDeny)
		}
		if _, err := ParseSchedule(s.Schedule); err != nil {
			fail(w.Origin, "sync window %q: %v", name, err)
		}
		if s.Duration <= 0 {
			fail(w.Origin, "sync window %q: duration must be positive", name)
		}
		if _, err := time.LoadLocation(s.TimeZone); err != nil {
			fail(w.Origin, "sync window %q: timeZone %q: %v", name, s.TimeZone, err)
		}
		if len(s.Applications) == 0 && len(s.Hosts) == 0 && len(s.Labels) == 0 {
			fail(w.Origin, "sync window %q: set at least one of applications, hosts or labels (use applications: [\"*\"] for every app)", name)
		}
		for _, g := range append(append([]string{}, s.Applications...), s.Hosts...) {
			if _, err := path.Match(g, ""); err != nil {
				fail(w.Origin, "sync window %q: invalid glob %q", name, g)
			}
		}
	}
	return errors.Join(errs...)
}

// ParseSchedule parses a standard five-field cron expression, or a descriptor
// such as @daily.
func ParseSchedule(s string) (cron.Schedule, error) {
	sched, err := cron.ParseStandard(s)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: %w", s, err)
	}
	return sched, nil
}

// dependencyCycle returns the names around one dependsOn cycle, or nil.
func (o *Objects) dependencyCycle() []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(name string) []string
	visit = func(name string) []string {
		switch state[name] {
		case visiting:
			for i, n := range stack {
				if n == name {
					return append(append([]string{}, stack[i:]...), name)
				}
			}
		case done:
			return nil
		}
		a := o.Application(name)
		if a == nil {
			return nil
		}
		state[name] = visiting
		stack = append(stack, name)
		for _, dep := range a.Spec.DependsOn {
			if c := visit(dep); c != nil {
				return c
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		return nil
	}
	for _, a := range o.Applications {
		if c := visit(a.Metadata.Name); c != nil {
			return c
		}
	}
	return nil
}

func hasScheme(u string, schemes ...string) bool {
	for _, s := range schemes {
		if strings.HasPrefix(u, s) {
			return true
		}
	}
	return false
}

// isWindowsAbs reports whether p looks like C:\ or C:/ (a Windows engine).
func isWindowsAbs(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}
