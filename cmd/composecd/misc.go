package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/source"
)

func newHostCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List hosts and whether they are reachable",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hs, err := cf.client().Hosts(ctx(cmd))
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), hs, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tSTATE\tENGINE\tDOCKER\tCOMPOSE\tAPPS\tNOTES")
				for _, h := range hs {
					engine := h.URL
					switch {
					case engine == "" && h.Context != "":
						engine = "context " + h.Context
					case engine == "":
						engine = "default"
					}
					var notes []string
					if h.Retired {
						notes = append(notes, "removed from Git; retained for prune")
					}
					if h.Maintenance {
						notes = append(notes, "maintenance: "+h.MaintenanceReason)
					}
					if len(h.Orphans) > 0 {
						notes = append(notes, "orphaned releases: "+strings.Join(h.Orphans, ","))
					}
					if h.Error != "" {
						notes = append(notes, firstLine(h.Error))
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", h.Name, h.State, engine, orDash(h.DockerVersion),
						orDash(h.ComposeVersion), h.Apps, strings.Join(notes, "; "))
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(list)
	var reason string
	maint := &cobra.Command{
		Use:   "maintenance NAME on|off",
		Short: "Turn a host's maintenance mode on or off",
		Long: `While a host is in maintenance, composecd doesn't sync, self-heal or prune
anything on it automatically; manual operations still work. Turning it off
reconciles the host's apps right away.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var on bool
			switch args[1] {
			case "on":
				on = true
			case "off":
			default:
				return fmt.Errorf("want on or off, not %q", args[1])
			}
			if err := cf.client().SetMaintenance(ctx(cmd), args[0], on, reason); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Maintenance mode of %s is %s.\n", args[0], args[1])
			return nil
		},
	}
	maint.Flags().StringVar(&reason, "reason", "", "why, shown in the status")

	cmd := &cobra.Command{Use: "host", Aliases: []string{"hosts"}, Short: "Inspect Docker hosts"}
	cmd.AddCommand(list, maint, newUnmanagedCmd(cf))
	return cmd
}

func newWindowCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sync windows, whether they're open, and the apps they match",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws, err := cf.client().Windows(ctx(cmd))
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), ws, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tKIND\tSCHEDULE\tDURATION\tMANUAL\tOPEN\tAPPLICATIONS")
				for _, win := range ws {
					open := "no, opens " + win.NextOpen.Local().Format("Mon 15:04")
					if win.Open {
						open = "until " + win.ClosesAt.Local().Format("Mon 15:04")
					}
					manual := "-"
					if win.ManualSync {
						manual = "allowed"
					}
					sched := win.Schedule
					if win.TimeZone != "" {
						sched += " " + win.TimeZone
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", win.Name, win.Kind, sched, win.Duration, manual, open,
						orDash(strings.Join(win.Applications, ",")))
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(list)
	cmd := &cobra.Command{Use: "window", Aliases: []string{"windows"}, Short: "Inspect sync windows"}
	cmd.AddCommand(list)
	return cmd
}

func newOperationCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	get := &cobra.Command{
		Use:   "get ID",
		Short: "Show an operation and its output",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid operation id %q", args[0])
			}
			op, err := cf.client().Operation(ctx(cmd), id)
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), op, func(w io.Writer) {
				fmt.Fprintf(w, "Operation:  #%d %s of %s (%s)\n", op.ID, op.Kind, op.App, op.Trigger)
				fmt.Fprintf(w, "Phase:      %s\n", op.Phase)
				if op.User != "" {
					fmt.Fprintf(w, "By:         %s\n", op.User)
				}
				if op.SourceRevision != "" {
					fmt.Fprintf(w, "Source:     %s\n", op.SourceRevision)
				}
				if op.ReleaseRevision > 0 {
					fmt.Fprintf(w, "Release:    revision %d\n", op.ReleaseRevision)
				}
				fmt.Fprintf(w, "Started:    %s\n", ago(op.StartedAt))
				if op.Message != "" {
					fmt.Fprintf(w, "Message:    %s\n", op.Message)
				}
				if op.Log != "" {
					fmt.Fprintf(w, "\nOUTPUT:\n%s\n", strings.TrimRight(op.Log, "\n"))
				}
			})
		},
	}
	format.register(get)

	var (
		listFormat outputFormat
		limit      int
	)
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List recent operations of every application",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ops, err := cf.client().Operations(ctx(cmd), "", limit)
			if err != nil {
				return err
			}
			return listFormat.write(cmd.OutOrStdout(), ops, func(w io.Writer) { printOperations(w, ops, true) })
		},
	}
	list.Flags().IntVar(&limit, "limit", 20, "how many operations to show")
	listFormat.register(list)

	cmd := &cobra.Command{Use: "operation", Aliases: []string{"op", "operations"}, Short: "Inspect sync and rollback operations"}
	cmd.AddCommand(get, list)
	return cmd
}

func newValidateCmd() *cobra.Command {
	var render, composeConfig bool
	var o controller.CheckOptions
	var outDir string
	cmd := &cobra.Command{
		Use:   "validate DIR",
		Short: "Check composecd objects, and render their charts with --render",
		Long: `Read every composecd object under DIR, as the server reads its root, and report
every problem. Also checks encrypted values filenames for SOPS metadata, without
decrypting them. Useful in the GitOps repository's CI. It doesn't contact hosts.

With --render, also expand ApplicationSets and render every Application's chart
from the Git checkout around DIR, as the server would, so a change that breaks a
chart fails before it is merged. lookup "composecd/application" sees the other
Applications' renders, and lookups of Docker objects return {}. Without
--sops-age-key-file, encrypted values render as placeholders, and
source.infisical adds no values. Applications whose source is another
repository are skipped. --compose also checks each rendered project with
"docker compose config", which needs the docker CLI but no engine.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			objs, err := config.LoadDir(args[0])
			if err != nil {
				return err
			}
			if err := source.ValidateSecrets(args[0], objs); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s: %d %s, %d %s and %d sync %s are valid.\n", args[0],
				len(objs.Hosts), plural(len(objs.Hosts), "host", "hosts"),
				len(objs.Applications), plural(len(objs.Applications), "application", "applications"),
				len(objs.SyncWindows), plural(len(objs.SyncWindows), "window", "windows"))
			if !render && !composeConfig {
				if len(objs.ApplicationSets) > 0 {
					fmt.Fprintf(out, "%d ApplicationSets will be expanded and fully validated by the server.\n", len(objs.ApplicationSets))
				}
				return nil
			}
			o.Compose = composeConfig
			results, sets, err := controller.Check(ctx(cmd), args[0], objs, o)
			failed := 0
			for _, s := range sets {
				switch {
				case s.Err != nil:
					failed++
					fmt.Fprintf(out, "FAIL  applicationset %s: %v\n", s.Name, s.Err)
				case s.Skipped != "":
					fmt.Fprintf(out, "skip  applicationset %s: %s\n", s.Name, s.Skipped)
				default:
					fmt.Fprintf(out, "ok    applicationset %s: %s\n", s.Name, orDash(strings.Join(s.Applications, ", ")))
				}
				for _, w := range s.Warnings {
					fmt.Fprintf(out, "      warning: %s\n", w)
				}
			}
			if err != nil {
				return err
			}
			if outDir != "" {
				if err := os.MkdirAll(outDir, 0o755); err != nil {
					return err
				}
			}
			rendered := 0
			for _, r := range results {
				where := r.Name + " (" + r.Host + ")"
				switch {
				case r.Err != nil:
					failed++
					fmt.Fprintf(out, "FAIL  %s: %v\n", where, r.Err)
				case r.Skipped != "":
					fmt.Fprintf(out, "skip  %s: %s\n", where, r.Skipped)
				default:
					rendered++
					fmt.Fprintf(out, "ok    %s\n", where)
					if outDir != "" {
						if err := os.WriteFile(filepath.Join(outDir, r.Name+".yaml"), []byte(r.Manifest), 0o644); err != nil {
							return err
						}
					}
				}
				if len(r.Stubbed) > 0 {
					fmt.Fprintf(out, "      without secrets from %s\n", strings.Join(r.Stubbed, ", "))
				}
				for _, n := range r.Notes {
					fmt.Fprintf(out, "      note: %s\n", n)
				}
			}
			fmt.Fprintf(out, "%d of %d %s rendered.\n", rendered, len(results), plural(len(results), "application", "applications"))
			if failed > 0 {
				return fmt.Errorf("%d %s failed", failed, plural(failed, "check", "checks"))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&render, "render", false, "expand ApplicationSets and render every Application's chart")
	f.BoolVar(&composeConfig, "compose", false, "render, then check each project with docker compose config (implies --render)")
	f.StringVar(&o.RepoURL, "repo-url", "", "the URL of the repository around DIR, so Applications naming it render from the checkout")
	f.StringVar(&o.AgeKeyFile, "sops-age-key-file", "", "age identity to decrypt SOPS values with; without it they render as placeholders")
	f.StringVar(&outDir, "output-dir", "", "write each rendered manifest to DIR/NAME.yaml")
	return cmd
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func newAppSetCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	list := &cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List ApplicationSets, generated apps and errors", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sets, err := cf.client().ApplicationSets(ctx(cmd))
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), sets, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tGENERATED\tREADY\tROLLOUT\tAPPLICATIONS\tERROR")
				for _, s := range sets {
					rollout := "-"
					if s.Rollout != nil {
						rollout = s.Rollout.Summary()
					}
					fmt.Fprintf(tw, "%s\t%d\t%t\t%s\t%s\t%s\n", s.Name, s.Generated, s.Ready, rollout, orDash(strings.Join(s.Applications, ",")), s.Error)
				}
				_ = tw.Flush()
			})
		}}
	format.register(list)
	var rolloutFormat outputFormat
	rollout := &cobra.Command{Use: "rollout NAME", Short: "Show where an ApplicationSet's rolling sync stands, step by step", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sets, err := cf.client().ApplicationSets(ctx(cmd))
			if err != nil {
				return err
			}
			i := slices.IndexFunc(sets, func(s controller.ApplicationSetStatus) bool { return s.Name == args[0] })
			if i < 0 {
				return fmt.Errorf("applicationset %q not found", args[0])
			}
			r := sets[i].Rollout
			if r == nil {
				return fmt.Errorf("applicationset %q has no rolling sync strategy", args[0])
			}
			return rolloutFormat.write(cmd.OutOrStdout(), r, func(w io.Writer) {
				fmt.Fprintf(w, "%s: %s\n", args[0], r.Summary())
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "STEP\tMAX\tAPPLICATION\tSTATE\tMESSAGE")
				for n, step := range r.Steps {
					for _, a := range step.Applications {
						fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\n", n+1, step.MaxUpdate, a.Name, a.State, orDash(a.Message))
					}
				}
				for _, name := range r.Unselected {
					fmt.Fprintf(tw, "-\t-\t%s\t-\tno step selects it; sync it manually\n", name)
				}
				_ = tw.Flush()
			})
		}}
	rolloutFormat.register(rollout)
	cmd := &cobra.Command{Use: "appset", Aliases: []string{"applicationset", "applicationsets"}, Short: "Inspect ApplicationSets"}
	cmd.AddCommand(list, rollout)
	return cmd
}
