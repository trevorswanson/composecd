package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/yaml"
)

// savedLogin is what `composecd login` stores, so later commands need no flags.
type savedLogin struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func loginFile() (string, error) {
	if f := os.Getenv("COMPOSECD_LOGIN_FILE"); f != "" {
		return f, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "composecd", "login.yaml"), nil
}

func readLogin() savedLogin {
	var l savedLogin
	f, err := loginFile()
	if err != nil {
		return l
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return l
	}
	_ = yaml.Unmarshal(data, &l)
	return l
}

// readSecret reads a password or token: hidden from a terminal, or the first
// line of stdin otherwise (for scripts: echo "$TOKEN" | composecd login ...).
func readSecret(cmd *cobra.Command, prompt string) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(cmd.ErrOrStderr(), prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func newPassword(cmd *cobra.Command) (string, error) {
	in := cmd.InOrStdin()
	pw, err := readSecret(cmd, "Password: ")
	if err != nil {
		return "", err
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		again, err := readSecret(cmd, "Again: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("the passwords don't match")
		}
	}
	return pw, nil
}

func newLoginCmd(cf *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Save the server and an API token for later commands",
		Long: `Ask for an API token (or read it from stdin), check it against the server
(--server), and save both, so later commands work without --server and --token.
$COMPOSECD_SERVER and $COMPOSECD_TOKEN still take precedence.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token := cf.token
			if !cmd.Flags().Changed("token") {
				var err error
				if token, err = readSecret(cmd, "API token: "); err != nil {
					return err
				}
			}
			c := cf.client()
			c.Token = token
			me, err := c.Me(ctx(cmd))
			if err != nil {
				return err
			}
			f, err := loginFile()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
				return err
			}
			data, _ := yaml.Marshal(savedLogin{Server: c.Server, Token: token})
			if err := os.WriteFile(f, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Signed in to %s as %s (%s). Saved to %s.\n", c.Server, me.Name, me.Role, f)
			return nil
		},
	}
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the saved server and token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := loginFile()
			if err != nil {
				return err
			}
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Signed out.")
			return nil
		},
	}
}

func newWhoamiCmd(cf *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the token belongs to",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			me, err := cf.client().Me(ctx(cmd))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s, via %s) on %s\n", me.Name, me.Role, me.Via, cf.server)
			return nil
		},
	}
}

func newTokenCmd(cf *clientFlags) *cobra.Command {
	var role string
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an API token and print it (it can't be shown again)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tok, err := cf.client().CreateToken(ctx(cmd), args[0], role)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			return nil
		},
	}
	create.Flags().StringVar(&role, "role", "readonly", "admin or readonly")

	var format outputFormat
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List API tokens",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ts, err := cf.client().Tokens(ctx(cmd))
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), ts, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tROLE\tCREATED\tBY\tLAST USED")
				for _, t := range ts {
					used := "never"
					if t.LastUsedAt != nil {
						used = ago(*t.LastUsedAt)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Role, t.CreatedAt.Local().Format(time.DateOnly), orDash(t.CreatedBy), used)
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(list)

	revoke := &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke an API token, and end browser sessions signed in with it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cf.client().RevokeToken(ctx(cmd), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Token %s revoked.\n", args[0])
			return nil
		},
	}
	cmd := &cobra.Command{Use: "token", Aliases: []string{"tokens"}, Short: "Manage API tokens (admin)"}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

func newUserCmd(cf *clientFlags) *cobra.Command {
	var role string
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a local user (prompts for the password, or reads it from stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := newPassword(cmd)
			if err != nil {
				return err
			}
			if err := cf.client().AddUser(ctx(cmd), args[0], role, pw); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "User %s (%s) added.\n", args[0], role)
			return nil
		},
	}
	add.Flags().StringVar(&role, "role", "readonly", "admin or readonly")

	passwd := &cobra.Command{
		Use:   "passwd NAME",
		Short: "Change a local user's password (signs them out everywhere)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := newPassword(cmd)
			if err != nil {
				return err
			}
			if err := cf.client().SetPassword(ctx(cmd), args[0], pw); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Password of %s changed.\n", args[0])
			return nil
		},
	}

	del := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a local user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cf.client().DeleteUser(ctx(cmd), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "User %s deleted.\n", args[0])
			return nil
		},
	}

	var format outputFormat
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List local users",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			us, err := cf.client().Users(ctx(cmd))
			if err != nil {
				return err
			}
			return format.write(cmd.OutOrStdout(), us, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tROLE\tCREATED")
				for _, u := range us {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Name, u.Role, u.CreatedAt.Local().Format(time.DateOnly))
				}
				_ = tw.Flush()
			})
		},
	}
	format.register(list)
	cmd := &cobra.Command{Use: "user", Aliases: []string{"users"}, Short: "Manage local users (admin)"}
	cmd.AddCommand(add, passwd, del, list)
	return cmd
}
