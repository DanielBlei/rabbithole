// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// The login gate's escape hatches live here rather than in the web UI or the
// config file: they need shell access to the machine holding the database,
// the same trust as editing it by hand, so nothing reachable over the network
// and no config edit can reopen a secured instance.
var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Inspect, reset or switch off the web UI's login",
}

var (
	resetUsername      string
	resetPasswordStdin bool
)

func init() {
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the web UI asks for a login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
				return authStatus(ctx, db, cmd.OutOrStdout())
			})
		},
	}
	resetCmd := &cobra.Command{
		Use:   "reset",
		Short: "Set a new password, for when the old one is forgotten",
		Long: "Set a new password for the web UI and log every browser out. It is asked for twice\n" +
			"and not echoed; --password-stdin reads it from the first line of stdin instead, for scripts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
				read := func() (string, error) {
					return readNewPassword(cmd.InOrStdin(), cmd.ErrOrStderr(), resetPasswordStdin)
				}
				return authReset(ctx, db, resetUsername, read, cmd.OutOrStdout())
			})
		},
	}
	resetCmd.Flags().StringVar(&resetUsername, "username", "", "change the username too (default: keep it)")
	resetCmd.Flags().BoolVar(&resetPasswordStdin, "password-stdin", false,
		"read the password from the first line of stdin rather than prompting")
	disableCmd := &cobra.Command{
		Use:   "disable",
		Short: "Switch the login off, leaving the web UI open to anyone who can reach it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
				return authDisable(ctx, db, cmd.OutOrStdout())
			})
		},
	}
	authCmd.AddCommand(statusCmd, resetCmd, disableCmd)
	rootCmd.AddCommand(authCmd)
}

func authStatus(ctx context.Context, db *store.Store, out io.Writer) error {
	st, err := db.AuthState(ctx)
	if err != nil {
		return err
	}
	switch st.Mode {
	case store.AuthInitial:
		_, err = fmt.Fprintf(out, "login: default (%s / %s), no password set yet\n",
			store.DefaultUsername, store.DefaultPassword)
	case store.AuthEnabled:
		_, err = fmt.Fprintf(out, "login: on, user %s\n", st.Username)
	default:
		_, err = fmt.Fprintln(out, "login: off, the web UI is open to anyone who can reach it")
	}
	return err
}

// authReset sets the new password straight away rather than going back to the
// default login, so a reset never leaves a window in which anyone who reaches
// the port could claim the instance.
func authReset(ctx context.Context, db *store.Store, username string, read func() (string, error),
	out io.Writer,
) error {
	if username = strings.TrimSpace(username); username == "" {
		st, err := db.AuthState(ctx)
		if err != nil {
			return err
		}
		username = st.Username
	}
	password, err := read()
	if err != nil {
		return err
	}
	if err := db.SetPassword(ctx, username, password); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "password set for %s; every browser has been logged out\n", username)
	return err
}

func authDisable(ctx context.Context, db *store.Store, out io.Writer) error {
	if err := db.DisableAuth(ctx); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, "login off: set a password again from the web UI's settings")
	return err
}

// readNewPassword takes the new password from the terminal, typed twice with
// no echo, or from the first line of in when fromStdin is set.
func readNewPassword(in io.Reader, prompt io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		if password := strings.TrimRight(line, "\r\n"); password != "" {
			return password, nil
		}
		return "", errors.New("no password on stdin")
	}
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("stdin is not a terminal: pipe the password in with --password-stdin")
	}
	ask := func(label string) (string, error) {
		_, _ = fmt.Fprint(prompt, label)
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(prompt)
		if err != nil {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		return string(b), nil
	}
	first, err := ask("New password: ")
	if err != nil {
		return "", err
	}
	again, err := ask("Retype new password: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the two passwords don't match")
	}
	return first, nil
}
