package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

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
	// Strategy, when set, rolls a change out across the generated
	// Applications progressively instead of syncing them all at once.
	Strategy *Strategy `yaml:"strategy,omitempty" json:"strategy,omitempty"`
}

// StrategyRollingSync is the only Strategy type.
const StrategyRollingSync = "RollingSync"

// Strategy is how automatic syncs of a set's Applications are sequenced.
type Strategy struct {
	Type        string       `yaml:"type" json:"type"`
	RollingSync *RollingSync `yaml:"rollingSync,omitempty" json:"rollingSync,omitempty"`
}

// RollingSync syncs the Applications step by step: an Application auto-syncs
// only once every Application of the earlier steps is Synced and Healthy, and
// while fewer than its step's maxUpdate Applications are updating or not
// healthy. A failed sync or an unhealthy Application halts the rollout.
type RollingSync struct {
	// Steps are matched in order; an Application belongs to the first step
	// that selects it. Without steps, every Application is in one step.
	// Applications no step selects are never synced automatically.
	Steps []RollingSyncStep `yaml:"steps,omitempty" json:"steps,omitempty"`
	// MaxUpdate is the default of steps that don't set their own (default
	// 100%).
	MaxUpdate *MaxUpdate `yaml:"maxUpdate,omitempty" json:"maxUpdate,omitempty"`
	// MinHealthyTime is how long an Application must stay Healthy after it
	// syncs before the rollout moves on, so a service that crashes shortly
	// after starting halts it (default 0).
	MinHealthyTime Duration `yaml:"minHealthyTime,omitempty" json:"minHealthyTime,omitempty"`
}

// RollingSyncStep selects Applications by their labels.
type RollingSyncStep struct {
	MatchExpressions []MatchExpression `yaml:"matchExpressions,omitempty" json:"matchExpressions,omitempty"`
	// MaxUpdate is how many of the step's Applications may update at once: a
	// number, or a percentage of the step's Applications (rounded down, at
	// least 1). 0 holds the step: none of its Applications auto-sync.
	MaxUpdate *MaxUpdate `yaml:"maxUpdate,omitempty" json:"maxUpdate,omitempty"`
}

// MatchExpression is a label requirement, as in Kubernetes: operator In,
// NotIn, Exists or DoesNotExist.
type MatchExpression struct {
	Key      string   `yaml:"key" json:"key"`
	Operator string   `yaml:"operator" json:"operator"`
	Values   []string `yaml:"values,omitempty" json:"values,omitempty"`
}

// Matches reports whether labels satisfy e.
func (e MatchExpression) Matches(labels map[string]string) bool {
	v, ok := labels[e.Key]
	switch e.Operator {
	case "In":
		return ok && slices.Contains(e.Values, v)
	case "NotIn":
		return !ok || !slices.Contains(e.Values, v)
	case "Exists":
		return ok
	case "DoesNotExist":
		return !ok
	}
	return false
}

// Selects reports whether s selects an Application with labels. A step
// without expressions selects every Application.
func (s RollingSyncStep) Selects(labels map[string]string) bool {
	for _, e := range s.MatchExpressions {
		if !e.Matches(labels) {
			return false
		}
	}
	return true
}

// MaxUpdate is a count ("1") or a percentage ("50%").
type MaxUpdate struct {
	Value   int
	Percent bool
}

// Of returns how many of n Applications m allows at once.
func (m *MaxUpdate) Of(n int) int {
	switch {
	case m == nil:
		return n
	case !m.Percent:
		return m.Value
	case m.Value == 0:
		return 0
	}
	return max(1, n*m.Value/100)
}

func (m MaxUpdate) String() string {
	if m.Percent {
		return strconv.Itoa(m.Value) + "%"
	}
	return strconv.Itoa(m.Value)
}

func parseMaxUpdate(s string) (MaxUpdate, error) {
	num, percent := strings.CutSuffix(s, "%")
	v, err := strconv.Atoi(num)
	if err != nil || v < 0 || (percent && v > 100) {
		return MaxUpdate{}, fmt.Errorf("invalid maxUpdate %q: want a count or a percentage such as 1 or 50%%", s)
	}
	return MaxUpdate{Value: v, Percent: percent}, nil
}

func (m *MaxUpdate) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := parseMaxUpdate(s)
	*m = v
	return err
}

func (m MaxUpdate) MarshalYAML() (interface{}, error) {
	if m.Percent {
		return m.String(), nil
	}
	return m.Value, nil
}

func (m MaxUpdate) MarshalJSON() ([]byte, error) {
	if m.Percent {
		return json.Marshal(m.String())
	}
	return json.Marshal(m.Value)
}

func (m *MaxUpdate) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	v, err := parseMaxUpdate(s)
	*m = v
	return err
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
		if st := set.Spec.Strategy; st != nil {
			switch {
			case st.Type != StrategyRollingSync:
				fail("strategy.type must be %s", StrategyRollingSync)
			case st.RollingSync == nil:
				fail("strategy.rollingSync is required")
			default:
				if st.RollingSync.MinHealthyTime < 0 {
					fail("strategy.rollingSync.minHealthyTime must not be negative")
				}
				for i, step := range st.RollingSync.Steps {
					for j, e := range step.MatchExpressions {
						where := fmt.Sprintf("strategy.rollingSync.steps[%d].matchExpressions[%d]", i, j)
						if e.Key == "" {
							fail("%s: key is required", where)
						}
						switch e.Operator {
						case "In", "NotIn":
							if len(e.Values) == 0 {
								fail("%s: operator %s needs values", where, e.Operator)
							}
						case "Exists", "DoesNotExist":
							if len(e.Values) > 0 {
								fail("%s: operator %s takes no values", where, e.Operator)
							}
						default:
							fail("%s: operator must be In, NotIn, Exists or DoesNotExist", where)
						}
					}
				}
			}
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
