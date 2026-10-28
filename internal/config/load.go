package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Objects are every composecd object read from a directory.
type Objects struct {
	Hosts           []*Host
	Applications    []*Application
	SyncWindows     []*SyncWindow
	ApplicationSets []*ApplicationSet
}

// Host returns the host named name, or nil.
func (o *Objects) Host(name string) *Host {
	for _, h := range o.Hosts {
		if h.Metadata.Name == name {
			return h
		}
	}
	return nil
}

// Application returns the application named name, or nil.
func (o *Objects) Application(name string) *Application {
	for _, a := range o.Applications {
		if a.Metadata.Name == name {
			return a
		}
	}
	return nil
}

// LoadDir reads every *.yaml and *.yml file under dir (recursively, skipping
// hidden directories), decodes the composecd objects in them and validates the
// set. YAML documents with another apiVersion are ignored, so the directory may
// hold other files too.
func LoadDir(dir string) (*Objects, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); ext == ".yaml" || ext == ".yml" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	objs := &Objects{}
	var errs []error
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(dir, f)
		if err := objs.decode(filepath.ToSlash(rel), data); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := objs.ValidateDefinitions(); err != nil {
		return nil, err
	}
	return objs, nil
}

// Parse decodes the composecd objects in one YAML stream named name and
// validates them.
func Parse(name string, data []byte) (*Objects, error) {
	objs := &Objects{}
	if err := objs.decode(name, data); err != nil {
		return nil, err
	}
	if err := objs.ValidateDefinitions(); err != nil {
		return nil, err
	}
	return objs, nil
}

type header struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// decode adds the objects in one YAML stream to o.
func (o *Objects) decode(name string, data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		origin := fmt.Sprintf("%s:%d", name, doc.Content[0].Line)
		// Read apiVersion and kind straight from the node: decoding the whole
		// document could fail (on duplicate keys, say), and a composecd object
		// that fails to decode must be reported, not skipped, or its app would
		// silently disappear (and be pruned).
		h := readHeader(doc.Content[0])
		if !strings.HasPrefix(h.APIVersion, "composecd/") {
			continue
		}
		if h.APIVersion != APIVersion {
			return fmt.Errorf("%s: unsupported apiVersion %q (want %s)", origin, h.APIVersion, APIVersion)
		}
		switch h.Kind {
		case KindHost:
			var obj struct {
				header   `yaml:",inline"`
				Metadata Metadata `yaml:"metadata"`
				Spec     HostSpec `yaml:"spec"`
			}
			if err := strictDecode(&doc, &obj); err != nil {
				return fmt.Errorf("%s: %w", origin, err)
			}
			o.Hosts = append(o.Hosts, &Host{Metadata: obj.Metadata, Spec: obj.Spec, Origin: origin})
		case KindApplication:
			var obj struct {
				header   `yaml:",inline"`
				Metadata Metadata `yaml:"metadata"`
				Spec     AppSpec  `yaml:"spec"`
			}
			if err := strictDecode(&doc, &obj); err != nil {
				return fmt.Errorf("%s: %w", origin, err)
			}
			o.Applications = append(o.Applications, &Application{Metadata: obj.Metadata, Spec: obj.Spec, Origin: origin})
		case KindApplicationSet:
			var obj struct {
				header   `yaml:",inline"`
				Metadata Metadata           `yaml:"metadata"`
				Spec     ApplicationSetSpec `yaml:"spec"`
			}
			if err := strictDecode(&doc, &obj); err != nil {
				return fmt.Errorf("%s: %w", origin, err)
			}
			o.ApplicationSets = append(o.ApplicationSets, &ApplicationSet{Metadata: obj.Metadata, Spec: obj.Spec, Origin: origin})
		case KindSyncWindow:
			var obj struct {
				header   `yaml:",inline"`
				Metadata Metadata       `yaml:"metadata"`
				Spec     SyncWindowSpec `yaml:"spec"`
			}
			if err := strictDecode(&doc, &obj); err != nil {
				return fmt.Errorf("%s: %w", origin, err)
			}
			o.SyncWindows = append(o.SyncWindows, &SyncWindow{Metadata: obj.Metadata, Spec: obj.Spec, Origin: origin})
		default:
			return fmt.Errorf("%s: unknown kind %q (want %s, %s, %s or %s)", origin, h.Kind, KindHost, KindApplication, KindSyncWindow, KindApplicationSet)
		}
	}
}

// readHeader returns the first apiVersion and kind keys of a mapping node, or
// an empty header for anything else.
func readHeader(n *yaml.Node) header {
	var h header
	if n.Kind != yaml.MappingNode {
		return h
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
			continue
		}
		switch {
		case k.Value == "apiVersion" && h.APIVersion == "":
			h.APIVersion = v.Value
		case k.Value == "kind" && h.Kind == "":
			h.Kind = v.Value
		}
	}
	return h
}

// strictDecode decodes n into v, rejecting fields v doesn't have.
func strictDecode(n *yaml.Node, v interface{}) error {
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// cleanRelative checks that p is a relative slash path that stays inside its
// base directory, and returns it cleaned ("" for the base itself).
func cleanRelative(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.Contains(p, `\`) || path.IsAbs(p) || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("%q must be a relative path with / separators", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%q must not leave its directory", p)
	}
	if c == "." {
		return "", nil
	}
	return c, nil
}
