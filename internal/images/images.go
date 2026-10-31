// Package images finds the images of a compose manifest's services, pins
// moving tags to registry digests, and resolves tags to digests.
package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
}

// Eligible is true when the service's tag may be pinned to a digest.
func (s Service) Eligible() bool { return s.Enabled && s.Skip == "" && !s.Pinned }

// Checked is true when the service's tag is resolved: to pin it, or to report
// that Git's pin is behind the tag.
func (s Service) Checked() bool { return s.Enabled && s.Skip == "" && s.Ref.Tag != "" }

// Services lists the images of manifest's services, sorted by service. enabled
// is the Application's setting, which a service's label overrides.
func Services(manifest string, enabled bool, label string) ([]Service, error) {
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
		s := Service{Name: name, Image: img.Value, Enabled: enabled}
		if v, ok := labelValue(value(svc, "labels"), label); ok {
			s.Enabled = v == "true"
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
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Pin is the digest a service's image is pinned to.
type Pin struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// PinManifest appends each service's pinned digest to its image, for services
// whose image is still pins[service].Image. It returns manifest unchanged when
// there is nothing to pin.
func PinManifest(manifest string, pins map[string]Pin) (string, error) {
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
		img.Value, img.Style, changed = img.Value+"@"+p.Digest, 0, true
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
			return strings.ToLower(v.Value), true
		}
	case yaml.SequenceNode:
		for _, item := range labels.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok && k == key {
				return strings.ToLower(v), true
			}
		}
	}
	return "", false
}

// Resolver resolves an image tag to the digest a registry serves for it.
type Resolver interface {
	Digest(ctx context.Context, ref Ref) (string, error)
}

// Registry resolves tags against remote registries with HEAD requests, which
// Docker Hub doesn't count against pull limits. Credentials come from the
// Docker config and its credential helpers, as for composetpl's charts.
type Registry struct {
	// PlainHTTP uses http:// for every registry. Registries on localhost always do.
	PlainHTTP bool
	// Log receives warnings.
	Log io.Writer
}

// Digest returns the digest of ref's tag: the index digest for a multi-arch
// image, which is what docker pins.
func (g *Registry) Digest(ctx context.Context, ref Ref) (string, error) {
	r, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return "", fmt.Errorf("invalid repository %s/%s: %w", ref.Registry, ref.Repository, err)
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
	desc, err := r.Resolve(ctx, ref.Tag)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref.Name(), err)
	}
	return desc.Digest.String(), nil
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
