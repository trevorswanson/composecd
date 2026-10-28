package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/trevorswanson/composetpl/pkg/release"
)

const (
	KindApplicationSet  = "ApplicationSet"
	LabelApplicationSet = "composecd/applicationset"
)

type ApplicationSet struct {
	Metadata Metadata           `yaml:"metadata" json:"metadata"`
	Spec     ApplicationSetSpec `yaml:"spec" json:"spec"`
	Origin   string             `yaml:"-" json:"origin,omitempty"`
}
type ApplicationSetSpec struct {
	Generators []Generator `yaml:"generators" json:"generators"`
	Template   Application `yaml:"template" json:"template"`
}
type Generator struct {
	GitDirectories *GitDirectories `yaml:"gitDirectories,omitempty" json:"gitDirectories,omitempty"`
	Hosts          *HostsGenerator `yaml:"hosts,omitempty" json:"hosts,omitempty"`
}
type GitDirectories struct {
	RepoURL        string   `yaml:"repoURL,omitempty" json:"repoURL,omitempty"`
	TargetRevision string   `yaml:"targetRevision,omitempty" json:"targetRevision,omitempty"`
	Directories    []string `yaml:"directories" json:"directories"`
	Exclude        []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}
type HostsGenerator struct {
	Selector map[string]string `yaml:"selector,omitempty" json:"selector,omitempty"`
}

func (o *Objects) validateApplicationSets() error {
	var errs []error
	names := map[string]bool{}
	for _, set := range o.ApplicationSets {
		fail := func(format string, args ...interface{}) {
			errs = append(errs, fmt.Errorf("%s: applicationset %q: %s", set.Origin, set.Metadata.Name, fmt.Sprintf(format, args...)))
		}
		if err := release.ValidateName(set.Metadata.Name); err != nil {
			fail("%v", err)
		}
		if names[set.Metadata.Name] {
			fail("name is already defined")
		}
		names[set.Metadata.Name] = true
		if err := release.ValidateLabels(set.Metadata.Labels); err != nil {
			fail("%v", err)
		}
		if len(set.Spec.Generators) == 0 {
			fail("at least one generator is required")
		}
		if set.Spec.Template.Metadata.Name == "" {
			fail("template.metadata.name is required")
		}
		for i, g := range set.Spec.Generators {
			if (g.GitDirectories == nil) == (g.Hosts == nil) {
				fail("generators[%d]: choose exactly one of gitDirectories or hosts", i)
				continue
			}
			if d := g.GitDirectories; d != nil {
				if len(d.Directories) == 0 {
					fail("generators[%d]: directories must not be empty", i)
				}
				for _, glob := range append(append([]string{}, d.Directories...), d.Exclude...) {
					if glob == "" || strings.HasPrefix(glob, "/") || isWindowsAbs(glob) || strings.Contains(glob, `\`) {
						fail("invalid directory glob %q", glob)
						continue
					}
					if _, err := cleanRelative(glob); err != nil {
						fail("invalid directory glob %q: %v", glob, err)
					}
					if _, err := path.Match(glob, ""); err != nil {
						fail("invalid directory glob %q: %v", glob, err)
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}

// ValidateDefinitions validates objects before server-side generator expansion.
// References to generated applications can only be checked on the expanded set.
func (o *Objects) ValidateDefinitions() error { return o.validate(len(o.ApplicationSets) > 0) }
