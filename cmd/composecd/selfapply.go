package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/trevorswanson/composecd/internal/selfapply"
)

// newSelfApplyCmd is what composecd's helper container runs to deploy
// composecd's own release (see docs/SELF.md). The server starts it; it isn't
// meant to be run by hand.
func newSelfApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "self-apply",
		Short:              "Deploy composecd's own release from a helper container",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := selfapply.Parse(args)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			cfg, err := o.Configuration(out)
			if err != nil {
				return err
			}
			rel, err := selfapply.Run(cmd.Context(), cfg, o)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s revision %d: %s\n", rel.Name, rel.Revision, rel.Description)
			return nil
		},
	}
}
