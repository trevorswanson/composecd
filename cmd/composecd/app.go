package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/store"
)

func newAppCmd(cf *clientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "app",
		Aliases: []string{"apps", "application"},
		Short:   "Inspect and operate on applications",
	}
	cmd.AddCommand(
		newAppListCmd(cf), newAppGetCmd(cf), newAppDiffCmd(cf), newAppSyncCmd(cf), newAppRollbackCmd(cf),
		newAppHistoryCmd(cf), newAppOperationsCmd(cf), newAppPauseCmd(cf), newAppResumeCmd(cf), newAppRefreshCmd(cf),
	)
	return cmd
}

func newAppListCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List applications with their sync and health status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			apps, err := cf.client().Apps(ctx(cmd))
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), apps, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tHOST\tSYNC\tHEALTH\tREVISION\tAUTO\tMESSAGE")
				for _, a := range apps {
					auto := "-"
					switch {
					case a.Automated && a.Paused:
						auto = "paused"
					case a.Automated:
						auto = "yes"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Host, a.Sync.Status, a.Health.Status,
						revision(a), auto, firstLine(message(a)))
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(cmd)
	return cmd
}

func revision(a controller.AppStatus) string {
	if a.Source.Chart != "" {
		chart := a.Source.Chart
		if a.Release != nil {
			chart = a.Release.Chart
		}
		if a.Sync.ValuesRevision != "" {
			chart += " / values " + short(a.Sync.ValuesRevision)
		}
		return chart
	}
	return short(a.Sync.Revision)
}

// message is the most useful one-line explanation of an app's state.
func message(a controller.AppStatus) string {
	switch {
	case a.Error != "":
		return a.Error
	case a.Blocked != "":
		return "waiting: " + a.Blocked
	case len(a.Sync.Reasons) > 0:
		return strings.Join(a.Sync.Reasons, "; ")
	}
	return a.Health.Message
}

func newAppGetCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Show an application's source, sync and health status and containers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cf.client().App(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), a, func(w io.Writer) { printApp(w, a) })
		},
	}
	format.register(cmd)
	return cmd
}

func printApp(w io.Writer, a *controller.AppStatus) {
	fmt.Fprintf(w, "Name:       %s\n", a.Name)
	fmt.Fprintf(w, "Host:       %s\n", a.Host)
	if s := a.Source; s.Chart != "" {
		fmt.Fprintf(w, "Chart:      %s %s\n", s.Chart, s.Version)
	} else {
		fmt.Fprintf(w, "Source:     %s @ %s, path %s\n", s.RepoURL, s.TargetRevision, orDot(s.Path))
	}
	auto := "manual"
	if a.Automated {
		auto = "automated"
	}
	if a.Paused {
		auto += " (paused: " + a.PausedReason + ")"
	}
	fmt.Fprintf(w, "Sync:       %s, %s\n", a.Sync.Status, auto)
	if a.Sync.ChartRef != "" {
		fmt.Fprintf(w, "Chart ref:  %s\n", a.Sync.ChartRef)
	}
	if vf := a.Source.ValuesFrom; vf != nil {
		fmt.Fprintf(w, "Values:     %s @ %s\n", vf.RepoURL, vf.TargetRevision)
	}
	if a.Sync.ValuesRevision != "" {
		fmt.Fprintf(w, "Values rev: %s (deployed: %s)\n", short(a.Sync.ValuesRevision), orNone(short(a.Sync.DeployedValuesRevision)))
	}
	if a.Sync.Revision != "" {
		fmt.Fprintf(w, "Revision:   %s (deployed: %s)\n", short(a.Sync.Revision), orNone(short(a.Sync.DeployedRevision)))
	}
	for _, r := range a.Sync.Reasons {
		fmt.Fprintf(w, "            - %s\n", r)
	}
	fmt.Fprintf(w, "Health:     %s", a.Health.Status)
	if a.Health.Message != "" {
		fmt.Fprintf(w, " (%s)", a.Health.Message)
	}
	fmt.Fprintln(w)
	if len(a.DependsOn) > 0 {
		fmt.Fprintf(w, "Depends on: %s\n", strings.Join(a.DependsOn, ", "))
	}
	if a.Blocked != "" {
		fmt.Fprintf(w, "Waiting:    %s\n", a.Blocked)
	}
	if r := a.Retry; r != nil {
		next := "no retries left"
		if !r.NextAt.IsZero() {
			next = "next " + time.Until(r.NextAt).Round(time.Second).String() + " from now"
		}
		fmt.Fprintf(w, "Retries:    %d failures, limit %d, %s\n", r.Failures, r.Limit, next)
	}
	if r := a.Release; r != nil {
		fmt.Fprintf(w, "Release:    revision %d, %s, %s, %s\n", r.Revision, r.Status, r.Chart, ago(r.LastDeployed))
	}
	if op := a.Operation; op != nil {
		fmt.Fprintf(w, "Operation:  #%d %s (%s) %s %s", op.ID, op.Kind, op.Trigger, op.Phase, ago(op.StartedAt))
		if op.Message != "" {
			fmt.Fprintf(w, ": %s", firstLine(op.Message))
		}
		fmt.Fprintln(w)
	}
	if a.Error != "" {
		fmt.Fprintf(w, "Error:      %s\n", a.Error)
	}
	if len(a.Health.Containers) > 0 {
		fmt.Fprintln(w, "\nCONTAINERS:")
		tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSERVICE\tSTATE\tHEALTH\tSTATUS\tIMAGE")
		for _, c := range a.Health.Containers {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Service, c.State, orDash(c.Health), c.Status, c.Image)
		}
		_ = tw.Flush()
	}
	if len(a.Sync.Drift) > 0 {
		fmt.Fprintln(w, "\nDRIFT:")
		tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tDRIFT\tCONTAINERS")
		for _, d := range a.Sync.Drift {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Service, d.Reason, strings.Join(d.Containers, ","))
		}
		_ = tw.Flush()
	}
}

func newAppDiffCmd(cf *clientFlags) *cobra.Command {
	var exitCode bool
	cmd := &cobra.Command{
		Use:   "diff NAME",
		Short: "Show how Git differs from the deployed release",
		Long: `Show, per compose resource, how the chart in Git differs from the deployed
release. With --exit-code, exit 1 when there are differences.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cf.client().App(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if a.Error != "" && a.Sync.Status == controller.Unknown {
				return fmt.Errorf("cannot diff %s: %s", a.Name, a.Error)
			}
			if a.Release == nil {
				fmt.Fprintf(w, "%s is not deployed yet; a sync installs it.\n", a.Name)
			}
			for _, ch := range a.Sync.Changes {
				fmt.Fprintf(w, "%s, %s, %s:\n%s\n", a.Name, ch.Resource, ch.Op, ch.Diff)
			}
			if a.Release != nil && len(a.Sync.Changes) == 0 {
				fmt.Fprintln(w, "No differences between Git and the deployed manifest.")
			}
			if exitCode && a.Sync.Status != controller.Synced {
				return &exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "exit 1 when the application is out of sync")
	return cmd
}

// waitFlags are shared by sync and rollback.
type waitFlags struct {
	wait    bool
	timeout time.Duration
	logs    bool
}

func (f *waitFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.wait, "wait", true, "wait for the operation to finish")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 15*time.Minute, "how long --wait waits")
	cmd.Flags().BoolVar(&f.logs, "logs", false, "print the operation's output when it finishes")
}

// finish waits for op (if asked) and reports it.
func (f *waitFlags) finish(cmd *cobra.Command, cf *clientFlags, op *store.Operation) error {
	w := cmd.OutOrStdout()
	if !f.wait {
		fmt.Fprintf(w, "Operation #%d started.\n", op.ID)
		return nil
	}
	c, cancel := context.WithTimeout(ctx(cmd), f.timeout)
	defer cancel()
	done, err := cf.client().Wait(c, op.ID, 2*time.Second)
	if err != nil {
		return fmt.Errorf("waiting for operation #%d: %w", op.ID, err)
	}
	if f.logs || done.Phase == store.PhaseFailed {
		if done.Log != "" {
			fmt.Fprint(w, strings.TrimRight(done.Log, "\n")+"\n")
		}
	}
	fmt.Fprintf(w, "Operation #%d %s: %s\n", done.ID, done.Phase, done.Message)
	if done.Phase != store.PhaseSucceeded {
		return &exitError{code: 1}
	}
	return nil
}

func newAppSyncCmd(cf *clientFlags) *cobra.Command {
	var wf waitFlags
	cmd := &cobra.Command{
		Use:   "sync NAME",
		Short: "Sync an application to Git now",
		Long: `Fetch the application's repository and deploy it as a new composetpl revision,
even if it is in sync or its automatic sync is paused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op, err := cf.client().Sync(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			return wf.finish(cmd, cf, op)
		},
	}
	wf.register(cmd)
	return cmd
}

func newAppRollbackCmd(cf *clientFlags) *cobra.Command {
	var wf waitFlags
	cmd := &cobra.Command{
		Use:   "rollback NAME [REVISION]",
		Short: "Roll an application back to an earlier release revision",
		Long: `Redeploy an earlier composetpl revision (see "app history") as a new revision;
with no REVISION, the one before the latest. An automated application is paused
afterwards so automatic sync doesn't undo the rollback; "app resume" re-enables it.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rev := 0
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 1 {
					return fmt.Errorf("invalid revision %q", args[1])
				}
				rev = n
			}
			op, err := cf.client().Rollback(ctx(cmd), args[0], rev)
			if err != nil {
				return err
			}
			return wf.finish(cmd, cf, op)
		},
	}
	wf.register(cmd)
	return cmd
}

func newAppHistoryCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:   "history NAME",
		Short: "List an application's composetpl release revisions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := cf.client().History(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), h, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "REVISION\tUPDATED\tSTATUS\tCHART\tSOURCE\tDESCRIPTION")
				for _, r := range h {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.Revision, r.LastDeployed.Local().Format(time.DateTime), r.Status,
						r.Chart, orDash(short(r.Labels[controller.LabelSourceRevision])), r.Description)
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(cmd)
	return cmd
}

func newAppOperationsCmd(cf *clientFlags) *cobra.Command {
	var (
		format outputFormat
		limit  int
	)
	cmd := &cobra.Command{
		Use:     "operations NAME",
		Aliases: []string{"ops"},
		Short:   "List an application's sync and rollback operations, newest first",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ops, err := cf.client().Operations(ctx(cmd), args[0], limit)
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), ops, func(w io.Writer) { printOperations(w, ops, false) })
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many operations to show")
	format.register(cmd)
	return cmd
}

func printOperations(w io.Writer, ops []*store.Operation, withApp bool) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	header := "ID\tKIND\tBY\tPHASE\tSOURCE\tRELEASE\tSTARTED\tMESSAGE"
	if withApp {
		header = "ID\tAPP\tKIND\tBY\tPHASE\tSOURCE\tRELEASE\tSTARTED\tMESSAGE"
	}
	fmt.Fprintln(tw, header)
	for _, op := range ops {
		rel := "-"
		if op.ReleaseRevision > 0 {
			rel = strconv.Itoa(op.ReleaseRevision)
		}
		app := ""
		if withApp {
			app = op.App + "\t"
		}
		who := op.User
		if who == "" {
			who = op.Trigger
		}
		fmt.Fprintf(tw, "%d\t%s%s\t%s\t%s\t%s\t%s\t%s\t%s\n", op.ID, app, op.Kind, who, op.Phase,
			orDash(short(op.SourceRevision)), rel, ago(op.StartedAt), firstLine(op.Message))
	}
	_ = tw.Flush()
}

func newAppPauseCmd(cf *clientFlags) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "pause NAME",
		Short: "Stop syncing an application automatically",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cf.client().Pause(ctx(cmd), args[0], reason); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Automatic sync of %s is paused.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, shown in the status")
	return cmd
}

func newAppResumeCmd(cf *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "resume NAME",
		Short: "Resume syncing an application automatically",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cf.client().Resume(ctx(cmd), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Automatic sync of %s is resumed.\n", args[0])
			return nil
		},
	}
}

func newAppRefreshCmd(cf *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh NAME",
		Short: "Fetch an application's repository and reconcile it now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cf.client().RefreshApp(ctx(cmd), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Refresh of %s requested.\n", args[0])
			return nil
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}
