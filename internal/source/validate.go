package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/sops"
)

// ValidateSecrets checks encrypted filenames without needing an age key or the
// SOPS binary. Besides files beneath dir, inspect local valueFiles relative to
// the surrounding Git root, since object roots usually sit beside charts.
func ValidateSecrets(dir string, objs *config.Objects) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir = root
	for p := root; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			root = p
			break
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var errs []error
	check := func(file string) {
		if seen[file] {
			return
		}
		seen[file] = true
		data, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if _, err = sops.Encrypted(file, data); err != nil {
			errs = append(errs, err)
		}
	}
	if err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if sops.Named(p) {
			real, err := inside(root, p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p, err))
			} else {
				check(real)
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}
	for _, a := range objs.Applications {
		s := a.Spec.Source
		if s.IsOCI() || s.RepoURL != "" {
			continue
		}
		// A validate-only object fixture need not contain its source chart.
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(s.Path), "Chart.yaml")); err != nil {
			continue
		}
		for _, vf := range s.ValueFiles {
			file, err := inside(root, filepath.Join(root, filepath.FromSlash(s.Path), filepath.FromSlash(vf)))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: valueFiles %q: %w", a.Origin, vf, err))
				continue
			}
			check(file)
		}
	}
	return errors.Join(errs...)
}
