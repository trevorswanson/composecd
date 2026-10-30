package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/trevorswanson/composecd/internal/appset"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/source"
)

type monitoringApplication struct {
	Name       string      `json:"name"`
	Host       string      `json:"host"`
	Monitoring interface{} `json:"monitoring"`
}

type monitoringInventory struct {
	Version      int                     `json:"version"`
	Applications []monitoringApplication `json:"applications"`
}

func newMonitoringCmd() *cobra.Command {
	var root string
	export := &cobra.Command{
		Use:   "export OBJECTS_DIR",
		Short: "Export local desired-state monitoring declarations as JSON, offline",
		Long: `Expand local ApplicationSets with the controller's generator and merge
chart defaults, ordered valueFiles and inline values. Emit only application
name, destination host and the monitoring values subtree. No Docker/server,
Git fetch, dependency vendoring, secret decryption or Infisical access occurs.

Only sources in this checkout with empty repoURL/targetRevision are supported.
Infisical is allowed only when scoped outside monitoring (e.g. into: secrets).
SOPS values, external/OCI sources, skipped chart directories and empty generator
output are errors. Monitoring values are literal, public configuration; never
put credentials in this subtree. Output is buffered until all apps succeed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := filepath.Abs(root)
			if err != nil {
				return err
			}
			repo, err = filepath.EvalSymlinks(repo)
			if err != nil {
				return err
			}
			objects, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			objects, err = filepath.EvalSymlinks(objects)
			if err != nil {
				return err
			}
			// LoadDir follows file symlinks; reject them before reading objects so a
			// checkout cannot cause this offline command to read operator-side files.
			if err := filepath.WalkDir(objects, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(repo, p)
				if err != nil || !filepath.IsLocal(rel) {
					return fmt.Errorf("objects must stay inside repo-root")
				}
				if d.Type()&fs.ModeSymlink != 0 {
					return fmt.Errorf("object symlinks are unsupported")
				}
				return nil
			}); err != nil {
				return err
			}
			objs, err := config.LoadDir(objects)
			if err != nil {
				return fmt.Errorf("invalid ComposeCD object definitions")
			}
			checkout := func(_ context.Context, url, rev string) (string, func(), error) {
				if url != "" || rev != "" {
					return "", nil, fmt.Errorf("offline generators require local repo and revision")
				}
				return repo, func() {}, nil
			}
			for _, set := range objs.ApplicationSets {
				apps, skipped, err := appset.Generate(cmd.Context(), set, objs.Hosts, checkout)
				if err != nil {
					return fmt.Errorf("applicationset %s: generation failed", set.Metadata.Name)
				}
				if len(apps) == 0 || len(skipped) > 0 {
					return fmt.Errorf("applicationset %s: empty output or skipped chart directories; refusing partial inventory", set.Metadata.Name)
				}
				objs.Applications = append(objs.Applications, apps...)
			}
			if err := objs.Validate(); err != nil {
				return fmt.Errorf("invalid expanded ComposeCD snapshot")
			}
			inventory := monitoringInventory{Version: 1, Applications: []monitoringApplication{}}
			sort.Slice(objs.Applications, func(i, j int) bool { return objs.Applications[i].Metadata.Name < objs.Applications[j].Metadata.Name })
			for _, app := range objs.Applications {
				monitoring, err := source.MonitoringValues(app, repo)
				if err != nil {
					return fmt.Errorf("application %s: %w", app.Metadata.Name, err)
				}
				inventory.Applications = append(inventory.Applications, monitoringApplication{app.Metadata.Name, app.Spec.Destination.Host, monitoring})
			}
			data, err := json.MarshalIndent(inventory, "", "  ")
			if err != nil {
				return fmt.Errorf("cannot serialize monitoring inventory")
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		},
	}
	export.Flags().StringVar(&root, "repo-root", ".", "local checkout containing charts and values")
	cmd := &cobra.Command{Use: "monitoring", Short: "Offline desired-state monitoring inventory"}
	cmd.AddCommand(export)
	return cmd
}
