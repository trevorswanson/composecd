package images

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Image update strategies.
const (
	// StrategyDigest pins a service's own tag to the digest its registry
	// serves. It is the default.
	StrategyDigest = "digest"
	// StrategySemver moves a service to the highest semantic version tag its
	// constraint allows.
	StrategySemver = "semver"
	// StrategyNewestBuild moves a service to the allowed tag whose image was
	// built last.
	StrategyNewestBuild = "newest-build"
)

// NewestBuildCandidates caps the tags newest-build reads image configs for,
// taking the newest by name first.
var NewestBuildCandidates = 50

// Policy is how a service's tag may move.
type Policy struct {
	// Strategy is digest (or ""), semver or newest-build.
	Strategy string `json:"strategy,omitempty"`
	// Constraint is a semver constraint, such as ~1.27 (semver only). The
	// default is the current tag's major version.
	Constraint string `json:"constraint,omitempty"`
	// AllowTags is a regexp a tag must match (semver and newest-build).
	AllowTags string `json:"allowTags,omitempty"`
	// IgnoreTags are path.Match globs of tags never picked.
	IgnoreTags []string `json:"ignoreTags,omitempty"`
	// Offer, when set, is a wider semver policy whose newer tags are only
	// reported, as upgrades an admin may approve; it never applies on its own.
	Offer *Policy `json:"offer,omitempty"`
}

// DefaultOfferConstraint is an offer's constraint when it names none: any
// newer release, majors included, since nothing is applied unapproved.
const DefaultOfferConstraint = "*"

// NewOffer is the offer policy with constraint, allowTags and ignoreTags
// (semver; constraint "" is DefaultOfferConstraint).
func NewOffer(constraint, allowTags string, ignoreTags []string) *Policy {
	if constraint == "" {
		constraint = DefaultOfferConstraint
	}
	return &Policy{Strategy: StrategySemver, Constraint: constraint, AllowTags: allowTags, IgnoreTags: ignoreTags}
}

// Approved is the policy that moves a service to exactly tag, an upgrade an
// admin approved, keeping p's offer.
func (p Policy) Approved(tag string) Policy {
	c := tag
	if v, err := semver.NewVersion(tag); err == nil {
		c = v.String()
	}
	return Policy{Strategy: StrategySemver, Constraint: "=" + c, AllowTags: "^" + regexp.QuoteMeta(tag) + "$", Offer: p.Offer}
}

// Name is the strategy, with digest for "".
func (p Policy) Name() string {
	if p.Strategy == "" {
		return StrategyDigest
	}
	return p.Strategy
}

// MovesTags is true for the strategies that pick a new tag.
func (p Policy) MovesTags() bool {
	return p.Strategy == StrategySemver || p.Strategy == StrategyNewestBuild
}

// Validate checks the strategy and that its fields parse.
func (p Policy) Validate() error {
	var errs []error
	switch p.Strategy {
	case "", StrategyDigest, StrategySemver, StrategyNewestBuild:
	default:
		return fmt.Errorf("strategy %q must be %s, %s or %s", p.Strategy, StrategyDigest, StrategySemver, StrategyNewestBuild)
	}
	if p.Constraint != "" {
		if p.Strategy != StrategySemver {
			errs = append(errs, errors.New("constraint only applies to the semver strategy"))
		} else if _, err := semver.NewConstraint(p.Constraint); err != nil {
			errs = append(errs, fmt.Errorf("constraint %q: %v", p.Constraint, err))
		}
	}
	if (p.AllowTags != "" || len(p.IgnoreTags) > 0) && !p.MovesTags() {
		errs = append(errs, errors.New("allowTags and ignoreTags only apply to the semver and newest-build strategies"))
	}
	if _, err := regexp.Compile(p.AllowTags); err != nil {
		errs = append(errs, fmt.Errorf("allowTags %q: %v", p.AllowTags, err))
	}
	if p.Strategy == StrategyNewestBuild && p.AllowTags == "" {
		errs = append(errs, errors.New("newest-build requires allowTags, so a repository's every tag isn't read"))
	}
	for _, g := range p.IgnoreTags {
		if _, err := path.Match(g, ""); err != nil {
			errs = append(errs, fmt.Errorf("ignoreTags %q: %v", g, err))
		}
	}
	if o := p.Offer; o != nil {
		switch {
		case o.Strategy != StrategySemver:
			errs = append(errs, fmt.Errorf("offer: strategy %q must be %s", o.Strategy, StrategySemver))
		case o.Offer != nil:
			errs = append(errs, errors.New("offer: an offer can't have an offer"))
		default:
			if err := o.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("offer: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// Newer is true when tag is a semantic version greater than current's; a
// current that isn't one is older than any.
func Newer(tag, current string) bool {
	v, err := semver.NewVersion(tag)
	if err != nil {
		return false
	}
	c, err := semver.NewVersion(current)
	return err != nil || v.GreaterThan(c)
}

// Fingerprint identifies the policy, so a pick made under another is redone.
func (p Policy) Fingerprint() string {
	return strings.Join(append([]string{p.Name(), p.Constraint, p.AllowTags}, p.IgnoreTags...), "\x00")
}

// ConstraintFor is the constraint semver applies to a service on tag current:
// the policy's, or current's major version (such as 1.x).
func (p Policy) ConstraintFor(current string) (string, error) {
	if p.Constraint != "" {
		return p.Constraint, nil
	}
	v, err := semver.NewVersion(current)
	if err != nil {
		return "", fmt.Errorf("the tag %q isn't a semantic version", current)
	}
	return fmt.Sprintf("%d.x", v.Major()), nil
}

// allowed returns a test that a tag matches AllowTags and no IgnoreTags glob.
// The policy must be valid.
func (p Policy) allowed() func(tag string) bool {
	var allow *regexp.Regexp
	if p.AllowTags != "" {
		allow = regexp.MustCompile(p.AllowTags)
	}
	return func(tag string) bool {
		if allow != nil && !allow.MatchString(tag) {
			return false
		}
		for _, g := range p.IgnoreTags {
			if ok, _ := path.Match(g, tag); ok {
				return false
			}
		}
		return true
	}
}

// IsSemver is true when tag parses as a semantic version (1.27, v1.27.3, ...).
func IsSemver(tag string) bool {
	_, err := semver.NewVersion(tag)
	return err == nil
}

// Accepts is true when the policy would pick tag over current: for semver, a
// newer version allowed by the constraint and tag filters; for newest-build,
// a tag the filters allow.
func (p Policy) Accepts(current, tag string) bool {
	if !p.allowed()(tag) {
		return false
	}
	switch p.Strategy {
	case StrategySemver:
		got, err := p.PickSemver(current, []string{tag})
		return err == nil && got == tag && tag != current
	case StrategyNewestBuild:
		return true
	}
	return false
}

// PickSemver returns the highest of tags that the policy allows and that is
// newer than current, or current when none is. Tags that aren't semantic
// versions are ignored, and prereleases are picked only when the constraint
// names one. Unless allowTags says which tags count, a tag with fewer version
// parts than current is ignored too: a date or build number (20230811) parses
// as a huge major version, so 3.0.1 would otherwise move to it.
func (p Policy) PickSemver(current string, tags []string) (string, error) {
	cur, err := semver.NewVersion(current)
	if err != nil {
		return "", fmt.Errorf("the tag %q isn't a semantic version", current)
	}
	expr, err := p.ConstraintFor(current)
	if err != nil {
		return "", err
	}
	c, err := semver.NewConstraint(expr)
	if err != nil {
		return "", fmt.Errorf("constraint %q: %v", expr, err)
	}
	prereleases, allowed := strings.Contains(expr, "-"), p.allowed()
	minParts := 0
	if p.AllowTags == "" {
		minParts = versionParts(current)
	}
	best, bestTag := cur, current
	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil || !allowed(t) || (v.Prerelease() != "" && !prereleases) || !c.Check(v) || versionParts(t) < minParts {
			continue
		}
		// Equal versions written differently (1.2 and 1.2.0) keep the first
		// by name, so the pick doesn't depend on the registry's order.
		if v.GreaterThan(best) || v.Equal(best) && bestTag != current && t < bestTag {
			best, bestTag = v, t
		}
	}
	return bestTag, nil
}

// versionParts counts the dotted numbers a version tag is written with,
// ignoring a v prefix, prerelease and build metadata: 3 for v1.27.3-alpine.
func versionParts(tag string) int {
	tag = strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	if i := strings.IndexAny(tag, "-+"); i >= 0 {
		tag = tag[:i]
	}
	return strings.Count(tag, ".") + 1
}

// NewestCandidates returns the tags newest-build reads image configs for: those
// the policy allows, newest by name first (build-10 before build-9), at most
// NewestBuildCandidates. current is always among them, so a pick is never
// older than what Git names.
func (p Policy) NewestCandidates(current string, tags []string) []string {
	var out []string
	allowed := p.allowed()
	for _, t := range tags {
		if t != current && allowed(t) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[j], out[i]) })
	if len(out) > NewestBuildCandidates {
		out = out[:NewestBuildCandidates]
	}
	return append(out, current)
}

// naturalLess orders strings with runs of digits compared as numbers.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digits(a), digits(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[da:], b[db:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// WithTag is image with its tag replaced by tag, written as image was:
// WithTag("nginx:1.27.1", "1.27.3") is nginx:1.27.3. image must not name a
// digest.
func WithTag(image, tag string) string {
	return Repo(image) + ":" + tag
}

// Repo is image without its tag or digest, as written: nginx for nginx:1.27.
func Repo(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		image = image[:i]
	}
	return image
}
