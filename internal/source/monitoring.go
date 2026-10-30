package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/sops"
	"github.com/trevorswanson/composetpl/pkg/values"
	"sigs.k8s.io/yaml"
)

// MonitoringValues resolves only declarative, non-secret monitoring values from
// a local chart. It shares composetpl's values.Merge precedence, but deliberately
// never calls Prepare: Prepare can fetch dependencies, decrypt files and contact
// Infisical. No templates are executed and no environment is interpolated.
// Unsupported sources fail closed rather than silently shrinking the inventory.
func MonitoringValues(app *config.Application, root string) (interface{}, error) {
	s := app.Spec.Source
	if s.IsOCI() || s.RepoURL != "" || s.TargetRevision != "" || s.ValuesFrom != nil {
		return nil, fmt.Errorf("offline monitoring requires a local source with no repoURL or targetRevision")
	}
	if s.Infisical != nil {
		into := s.Infisical.Into
		if into == "" || strings.Split(into, ".")[0] == "monitoring" {
			return nil, fmt.Errorf("Infisical must be scoped outside monitoring (for example into: secrets)")
		}
	}
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	dir, err := inside(root, filepath.Join(root, filepath.FromSlash(s.Path)))
	if err != nil {
		return nil, fmt.Errorf("chart path: %w", err)
	}
	if _, err := inside(root, filepath.Join(dir, "Chart.yaml")); err != nil {
		return nil, fmt.Errorf("chart requires Chart.yaml: %w", err)
	}
	merged := map[string]interface{}{}
	read := func(name string) error {
		file, err := inside(root, name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("cannot read values file")
		}
		encrypted, err := sops.Encrypted(file, data)
		if err != nil || encrypted {
			return fmt.Errorf("encrypted or invalid SOPS values are unsupported offline")
		}
		var current map[string]interface{}
		if err := yaml.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("invalid values YAML")
		}
		merged = values.Merge(merged, current)
		return nil
	}
	defaults := filepath.Join(dir, "values.yaml")
	if _, err := os.Lstat(defaults); err == nil {
		if err := read(defaults); err != nil {
			return nil, fmt.Errorf("chart values.yaml: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot stat chart values.yaml")
	}
	for _, file := range s.ValueFiles {
		if err := read(filepath.Join(dir, filepath.FromSlash(file))); err != nil {
			return nil, fmt.Errorf("valueFiles: %w", err)
		}
	}
	inline, err := normalize(s.Values)
	if err != nil {
		return nil, fmt.Errorf("invalid inline values")
	}
	merged = values.Merge(merged, inline)
	return merged["monitoring"], nil
}
