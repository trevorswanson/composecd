// Command composecd is a GitOps controller for composetpl charts: it keeps the
// Applications defined in a Git repository deployed as composetpl releases on
// one or more Docker hosts. The same binary is the server and its CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/trevorswanson/composecd/internal/client"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// exitError exits with code without printing (the command already reported).
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// clientFlags select the server CLI commands talk to.
type clientFlags struct {
	server string
	token  string
}

func (f *clientFlags) client() *client.Client { return client.New(f.server, f.token) }

func newRootCmd() *cobra.Command {
	cf := &clientFlags{}
	root := &cobra.Command{
		Use:   "composecd",
		Short: "GitOps for composetpl charts on Docker hosts",
		Long: `composecd keeps the Applications defined in a Git repository deployed as
composetpl releases on one or more Docker hosts, like Argo CD does for Helm
charts on Kubernetes.

Run "composecd server" to start the controller. The other commands talk to a
running server (--server, $COMPOSECD_SERVER) with an API token (--token,
$COMPOSECD_TOKEN); "composecd login" saves both.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Flags win over the environment, which wins over `composecd login`.
	saved := readLogin()
	fs := root.PersistentFlags()
	fs.StringVar(&cf.server, "server", envOr("COMPOSECD_SERVER", orDefault(saved.Server, "http://localhost:8080")),
		"composecd server URL [$COMPOSECD_SERVER, or as saved by login]")
	fs.StringVar(&cf.token, "token", envOr("COMPOSECD_TOKEN", saved.Token), "API token [$COMPOSECD_TOKEN, or as saved by login]")
	root.AddCommand(
		newServerCmd(),
		newSelfApplyCmd(),
		newValidateCmd(),
		newAppCmd(cf),
		newAppSetCmd(cf),
		newHostCmd(cf),
		newAdoptCmd(cf),
		newWindowCmd(cf),
		newOperationCmd(cf),
		newLoginCmd(cf),
		newLogoutCmd(),
		newWhoamiCmd(cf),
		newTokenCmd(cf),
		newUserCmd(cf),
		&cobra.Command{
			Use:   "refresh",
			Short: "Fetch Git and reconcile every application now",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := cf.client().Refresh(cmd.Context()); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Refresh requested.")
				return nil
			},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Args:  cobra.NoArgs,
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) },
		},
	)
	return root
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// outputFormat is the -o table|json|yaml flag.
type outputFormat struct{ format string }

func (o *outputFormat) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&o.format, "output", "o", "table", "output format: table, json or yaml")
}

// write prints v as JSON or YAML, or calls table for the human format.
func (o *outputFormat) write(w io.Writer, v interface{}, table func(io.Writer)) error {
	switch o.format {
	case "table":
		table(w)
		return nil
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out, err := yaml.JSONToYAML(data)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	}
	return fmt.Errorf("unknown output format %q (want table, json or yaml)", o.format)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func ctx(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}
