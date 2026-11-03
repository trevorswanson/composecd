// Package images finds the images of a compose manifest's services, pins
// moving tags to registry digests, picks newer tags by a strategy, and asks
// registries about tags and digests.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/trevorswanson/composetpl/pkg/output"
)

// DockerHub is the registry of images named without one.
const DockerHub = "docker.io"

// Ref is a parsed image reference, with Docker's defaults filled in.
type Ref struct {
	// Registry is the registry host[:port], e.g. docker.io or ghcr.io.
	Registry string
	// Repository is the path in the registry, e.g. library/nginx.
	Repository string
	// Tag is "latest" when the image names neither a tag nor a digest.
	Tag    string
	Digest string
}

// Name is registry/repository:tag, the same for every way of writing one
// image tag (nginx, docker.io/library/nginx:latest, ...).
func (r Ref) Name() string { return r.Registry + "/" + r.Repository + ":" + r.Tag }

// Parse parses an image reference as docker does: nginx means
// docker.io/library/nginx:latest.
func Parse(image string) (Ref, error) {
	var r Ref
	name := image
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name, r.Digest = name[:i], name[i+1:]
		if !strings.HasPrefix(r.Digest, "sha256:") || len(r.Digest) != len("sha256:")+64 {
			return Ref{}, fmt.Errorf("invalid image %q: bad digest", image)
		}
	}
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, r.Tag = name[:i], name[i+1:]
	}
	if name == "" || strings.ContainsAny(name, " \t$") || r.Tag == "" && strings.HasSuffix(image, ":") {
		return Ref{}, fmt.Errorf("invalid image %q", image)
	}
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repository = first, rest
	} else {
		r.Registry, r.Repository = DockerHub, name
		if !strings.Contains(name, "/") {
			r.Repository = "library/" + name
		}
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Service is one service's image in a manifest.
type Service struct {
	Name string `json:"service"`
	// Image is the image as the manifest has it.
	Image string `json:"image"`
	Ref   Ref    `json:"-"`
	// Pinned is true when the manifest already names a digest.
	Pinned bool `json:"pinned,omitempty"`
	// Enabled is true when image updates are on for the service.
	Enabled bool `json:"enabled"`
	// Skip says why the image can't be checked at all ("" when it can).
	Skip string `json:"skip,omitempty"`
	// Policy is how the tag may move: the Application's, with the service's
	// labels applied.
	Policy Policy `json:"policy"`
	// TagKey is the values key (or Chart.appVersion) that sets the tag, as
	// the service's label.tag-key names it, for write-back; "" lets
	// write-back trace it.
	TagKey string `json:"tagKey,omitempty"`
	// Warning says why the service's policy doesn't apply as written: its
	// labels are invalid (the digest strategy is used), or semver is asked of a
	// tag that isn't a semantic version (its digest is pinned).
	Warning string `json:"warning,omitempty"`
}

// Eligible is true when the service's tag may be pinned to a digest.
func (s Service) Eligible() bool { return s.Enabled && s.Skip == "" && !s.Pinned }

// Checked is true when the service's tag is resolved: to pin it, or to report
// that Git's pin is behind the tag.
func (s Service) Checked() bool { return s.Enabled && s.Skip == "" && s.Ref.Tag != "" }

// MovesTags is true when a strategy may move the service to another tag.
func (s Service) MovesTags() bool {
	return s.Eligible() && s.Policy.MovesTags() && (s.Policy.Strategy != StrategySemver || IsSemver(s.Ref.Tag))
}

// Services lists the images of manifest's services, sorted by service. enabled
// and policy are the Application's settings, which a service's label
// overrides: label ("true" or "false"), and label.strategy, label.constraint
// and label.allow-tags. label.tag-key is read into TagKey.
func Services(manifest string, enabled bool, policy Policy, label string) ([]Service, error) {
	doc, err := parse(manifest)
	if err != nil {
		return nil, err
	}
	var out []Service
	for name, svc := range services(doc) {
		img := value(svc, "image")
		if img == nil {
			continue
		}
		s := Service{Name: name, Image: img.Value, Enabled: enabled, Policy: policy}
		if label != "" {
			labels := value(svc, "labels")
			if v, ok := labelValue(labels, label); ok {
				s.Enabled = strings.EqualFold(v, "true")
			}
			if v, ok := labelValue(labels, label+".strategy"); ok && strings.ToLower(v) != s.Policy.Name() {
				// Another strategy starts afresh: the Application's constraint
				// and tag filters were written for its own.
				s.Policy = Policy{Strategy: strings.ToLower(v)}
			}
			if v, ok := labelValue(labels, label+".constraint"); ok {
				s.Policy.Constraint = v
			}
			if v, ok := labelValue(labels, label+".allow-tags"); ok {
				s.Policy.AllowTags = v
			}
			if v, ok := labelValue(labels, label+".tag-key"); ok {
				s.TagKey = strings.TrimSpace(v)
			}
			if err := s.Policy.Validate(); err != nil {
				s.Policy, s.Warning = Policy{}, "invalid "+label+" labels, using the digest strategy: "+err.Error()
			}
		}
		switch r, err := Parse(img.Value); {
		case strings.Contains(img.Value, "$"):
			s.Skip = "the image uses a variable"
		case value(svc, "build") != nil:
			s.Skip = "the service is built locally"
		case err != nil:
			s.Skip = err.Error()
		default:
			s.Ref, s.Pinned = r, r.Digest != ""
		}
		if s.Eligible() && s.Policy.Strategy == StrategySemver && !IsSemver(s.Ref.Tag) && s.Warning == "" {
			s.Warning = fmt.Sprintf("the tag %q isn't a semantic version, so semver can't move it; its digest is pinned", s.Ref.Tag)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Pin is the digest a service's image is pinned to, and the tag a strategy
// moved it to, if any.
type Pin struct {
	Image string `json:"image"`
	// Tag replaces Image's tag ("" keeps it).
	Tag    string `json:"tag,omitempty"`
	Digest string `json:"digest"`
}

// Override rewrites each service's image as pins say, for services whose image
// is still pins[service].Image: to repo:tag@digest when the pin moves the tag,
// or with the digest appended otherwise. It returns manifest unchanged when
// there is nothing to do.
func Override(manifest string, pins map[string]Pin) (string, error) {
	if len(pins) == 0 {
		return manifest, nil
	}
	doc, err := parse(manifest)
	if err != nil {
		return "", err
	}
	changed := false
	for name, svc := range services(doc) {
		p, ok := pins[name]
		img := value(svc, "image")
		if !ok || img == nil || img.Value != p.Image || strings.Contains(img.Value, "@") {
			continue
		}
		want := img.Value
		if p.Tag != "" {
			want = WithTag(want, p.Tag)
		}
		img.Value, img.Style, changed = want+"@"+p.Digest, 0, true
	}
	if !changed {
		return manifest, nil
	}
	out, err := output.Encode(doc.Content[0])
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func parse(manifest string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return nil, fmt.Errorf("parsing the manifest: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("parsing the manifest: not a mapping")
	}
	return &doc, nil
}

// services maps each service's name to its definition.
func services(doc *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	s := value(doc.Content[0], "services")
	if s == nil || s.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(s.Content); i += 2 {
		if s.Content[i+1].Kind == yaml.MappingNode {
			out[s.Content[i].Value] = s.Content[i+1]
		}
	}
	return out
}

func value(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// labelValue reads key from compose labels, written as a mapping or a list of
// key=value.
func labelValue(labels *yaml.Node, key string) (string, bool) {
	if labels == nil {
		return "", false
	}
	switch labels.Kind {
	case yaml.MappingNode:
		if v := value(labels, key); v != nil {
			return v.Value, true
		}
	case yaml.SequenceNode:
		for _, item := range labels.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok && k == key {
				return v, true
			}
		}
	}
	return "", false
}

// Resolver asks registries about images.
type Resolver interface {
	// Digest resolves ref's tag to the digest its registry serves for it.
	Digest(ctx context.Context, ref Ref) (string, error)
	// Tags lists ref's repository's tags, stopping after a cap; truncated is
	// true when it stopped before the end.
	Tags(ctx context.Context, ref Ref) (tags []string, truncated bool, err error)
	// Created is when ref's image (named by its Digest) was built, from its
	// config. For a multi-arch image it is the first platform's.
	Created(ctx context.Context, ref Ref) (time.Time, error)
}

// Registry resolves tags against remote registries with HEAD requests, which
// Docker Hub doesn't count against pull limits. Credentials come from the
// Docker config and its credential helpers, as for composetpl's charts.
type Registry struct {
	// PlainHTTP uses http:// for every registry. Registries on localhost always do.
	PlainHTTP bool
	// MaxTags caps the tags Tags lists (default DefaultMaxTags).
	MaxTags int
	// Log receives warnings.
	Log io.Writer
}

// DefaultMaxTags is how many tags Tags lists before it reports the list as
// truncated. Registries list tags in lexical order, a page at a time.
const DefaultMaxTags = 10000

// errEnough stops a tag listing at the cap.
var errEnough = errors.New("enough tags")

func (g *Registry) repository(ref Ref) (*remote.Repository, error) {
	r, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, fmt.Errorf("invalid repository %s/%s: %w", ref.Registry, ref.Repository, err)
	}
	if ref.Registry == DockerHub {
		r.Reference.Registry = "registry-1.docker.io"
	}
	r.PlainHTTP = g.PlainHTTP || isLocalhost(ref.Registry)
	var credFunc auth.CredentialFunc
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		inner := credentials.Credential(store)
		credFunc = func(ctx context.Context, host string) (auth.Credential, error) {
			got, err := inner(ctx, host)
			if err != nil {
				// A broken credential helper shouldn't block anonymous checks.
				g.warnf("reading Docker credentials for %s: %v; continuing anonymously", host, err)
				return auth.EmptyCredential, nil
			}
			return got, nil
		}
	} else {
		g.warnf("reading Docker config: %v; continuing anonymously", err)
	}
	r.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: credFunc}
	return r, nil
}

// Digest returns the digest of ref's tag: the index digest for a multi-arch
// image, which is what docker pins.
func (g *Registry) Digest(ctx context.Context, ref Ref) (string, error) {
	r, err := g.repository(ref)
	if err != nil {
		return "", err
	}
	desc, err := r.Resolve(ctx, ref.Tag)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref.Name(), err)
	}
	return desc.Digest.String(), nil
}

// Tags lists the tags of ref's repository, up to MaxTags.
func (g *Registry) Tags(ctx context.Context, ref Ref) ([]string, bool, error) {
	r, err := g.repository(ref)
	if err != nil {
		return nil, false, err
	}
	limit := g.MaxTags
	if limit <= 0 {
		limit = DefaultMaxTags
	}
	var tags []string
	truncated := false
	err = r.Tags(ctx, "", func(page []string) error {
		if len(tags) >= limit {
			truncated = true
			return errEnough
		}
		tags = append(tags, page...)
		return nil
	})
	if err != nil && !errors.Is(err, errEnough) {
		return nil, false, fmt.Errorf("listing the tags of %s/%s: %w", ref.Registry, ref.Repository, err)
	}
	if len(tags) > limit {
		tags, truncated = tags[:limit], true
	}
	return tags, truncated, nil
}

// imageManifest is the part of an image manifest or index Created reads.
type imageManifest struct {
	Manifests []ocispec.Descriptor `json:"manifests"`
	Config    ocispec.Descriptor   `json:"config"`
}

// Created reads the created time from the config of ref's image, by digest.
// For an index it reads the first platform's image, skipping attestations.
func (g *Registry) Created(ctx context.Context, ref Ref) (time.Time, error) {
	r, err := g.repository(ref)
	if err != nil {
		return time.Time{}, err
	}
	desc, rc, err := r.FetchReference(ctx, ref.Digest)
	if err != nil {
		return time.Time{}, fmt.Errorf("fetching %s@%s: %w", ref.Name(), Short(ref.Digest), err)
	}
	body, err := content.ReadAll(rc, desc)
	rc.Close()
	if err != nil {
		return time.Time{}, err
	}
	var m imageManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return time.Time{}, fmt.Errorf("reading %s@%s: %w", ref.Name(), Short(ref.Digest), err)
	}
	for _, d := range m.Manifests {
		if d.Platform != nil && d.Platform.OS == "unknown" {
			continue
		}
		if body, err = content.FetchAll(ctx, r, d); err != nil {
			return time.Time{}, fmt.Errorf("fetching %s@%s: %w", ref.Name(), Short(d.Digest.String()), err)
		}
		m = imageManifest{}
		if err := json.Unmarshal(body, &m); err != nil {
			return time.Time{}, fmt.Errorf("reading %s@%s: %w", ref.Name(), Short(d.Digest.String()), err)
		}
		break
	}
	if m.Config.Digest == "" {
		return time.Time{}, fmt.Errorf("%s@%s has no image config", ref.Name(), Short(ref.Digest))
	}
	body, err = content.FetchAll(ctx, r, m.Config)
	if err != nil {
		return time.Time{}, fmt.Errorf("fetching the config of %s: %w", ref.Name(), err)
	}
	var cfg struct {
		Created *time.Time `json:"created"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil || cfg.Created == nil {
		return time.Time{}, fmt.Errorf("the config of %s has no created time", ref.Name())
	}
	return *cfg.Created, nil
}

func (g *Registry) warnf(format string, args ...interface{}) {
	if g.Log != nil {
		fmt.Fprintf(g.Log, "WARNING: "+format+"\n", args...)
	}
}

func isLocalhost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Short abbreviates a digest for messages: sha256:0123456789ab.
func Short(digest string) string {
	if len(digest) > len("sha256:")+12 {
		return digest[:len("sha256:")+12]
	}
	return digest
}
