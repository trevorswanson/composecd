package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/trevorswanson/composecd/internal/config"
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
				fmt.Fprintln(tw, "NAME\tSTATE\tENGINE\tDOCKER\tCOMPOSE\tAPPS\tERROR")
				for _, h := range hs {
					engine := h.URL
					switch {
					case engine == "" && h.Context != "":
						engine = "context " + h.Context
					case engine == "":
						engine = "default"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", h.Name, h.State, engine, orDash(h.DockerVersion),
						orDash(h.ComposeVersion), h.Apps, firstLine(h.Error))
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(list)
	cmd := &cobra.Command{Use: "host", Aliases: []string{"hosts"}, Short: "Inspect Docker hosts"}
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
	return &cobra.Command{
		Use:   "validate DIR",
		Short: "Check the Host and Application objects in a directory",
		Long: `Read every composecd object under DIR, as the server reads its root, and report
every problem. Useful in the GitOps repository's CI. It doesn't render charts or
contact hosts.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			objs, err := config.LoadDir(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d %s and %d %s are valid.\n", args[0],
				len(objs.Hosts), plural(len(objs.Hosts), "host", "hosts"),
				len(objs.Applications), plural(len(objs.Applications), "application", "applications"))
			return nil
		},
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
