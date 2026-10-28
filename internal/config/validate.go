package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

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
			if len(s.ValueFiles) > 0 {
				fail(a.Origin, "application %q: valueFiles need a chart in Git (path); use values with an oci:// chart", name)
			}
		default:
			if s.Version != "" {
				fail(a.Origin, "application %q: version only applies to an oci:// chart", name)
			}
			if p, err := cleanRelative(s.Path); err != nil {
				fail(a.Origin, "application %q: path %v", name, err)
			} else {
				a.Spec.Source.Path = p
			}
			for i, vf := range s.ValueFiles {
				// Relative to the chart path, but may reach elsewhere in the repository.
				joined := path.Clean(path.Join(a.Spec.Source.Path, vf))
				if vf == "" || path.IsAbs(vf) || strings.Contains(vf, `\`) || joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
					fail(a.Origin, "application %q: valueFiles[%d] %q must be a file inside the chart's repository", name, i, vf)
				}
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
	}
	return errors.Join(errs...)
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
