package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newUnmanagedCmd(cf *clientFlags) *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{Use: "unmanaged NAME", Short: "List a host's unmanaged releases and plain Compose projects", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inventory, err := cf.client().Unmanaged(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), inventory, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tCHART / CONFIG FILES\tNOTES")
				for _, r := range inventory.Releases {
					kind := "release"
					if r.Orphaned {
						kind = "orphaned release"
					}
					note := "composecd adopt " + args[0] + " " + r.Name
					if r.Blocked != "" {
						note = r.Blocked
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, kind, r.Status, r.ChartRef, note)
				}
				for _, p := range inventory.Projects {
					fmt.Fprintf(tw, "%s\tcompose project\t%s\t%s\t%s\n", p.Name, p.Status, strings.Join(p.ConfigFiles, ", "), p.Hint)
				}
				_ = tw.Flush()
			})
		}}
	format.register(cmd)
	return cmd
}

func newAdoptCmd(cf *clientFlags) *cobra.Command {
	return &cobra.Command{Use: "adopt HOST RELEASE", Short: "Print an Application YAML for an existing composetpl release (admin)",
		Long: `Export the release name, chart source and saved user values as Application YAML.
This changes neither Git nor the release. Output contains plaintext values: review
and encrypt secrets before committing. Run app diff after committing, then sync.
Warnings go to stderr, so stdout can be redirected to a file.`, Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			adoption, err := cf.client().Adopt(ctx(cmd), args[0], args[1])
			if err != nil {
				return err
			}
			for _, warning := range adoption.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", warning)
			}
			_, err = io.WriteString(cmd.OutOrStdout(), adoption.YAML)
			return err
		}}
}
