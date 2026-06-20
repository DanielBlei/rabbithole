package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/ai-searcher/internal/config"
	"github.com/DanielBlei/ai-searcher/internal/store"
)

var itemCmd = &cobra.Command{
	Use:   "item",
	Short: "Record your own read/skip/rating/notes for a digest item, by link",
}

func init() {
	readCmd := &cobra.Command{
		Use:   "read <link>",
		Short: "Mark an item as read",
		Args:  cobra.ExactArgs(1),
		RunE:  statusRunE(store.StatusRead, "read"),
	}
	skipCmd := &cobra.Command{
		Use:   "skip <link>",
		Short: "Mark an item as skipped",
		Args:  cobra.ExactArgs(1),
		RunE:  statusRunE(store.StatusSkipped, "skipped"),
	}
	unreadCmd := &cobra.Command{
		Use:   "unread <link>",
		Short: "Reset an item back to unread",
		Args:  cobra.ExactArgs(1),
		RunE:  statusRunE(store.StatusUnread, "unread"),
	}
	rateCmd := &cobra.Command{
		Use:   "rate <link> <0-10>",
		Short: "Give an item your own relevance score",
		Args:  cobra.ExactArgs(2),
		RunE:  runRate,
	}
	noteCmd := &cobra.Command{
		Use:   "note <link> <text>...",
		Short: "Attach a free-text note to an item",
		Args:  cobra.MinimumNArgs(2),
		RunE:  runNote,
	}
	itemCmd.AddCommand(readCmd, skipCmd, unreadCmd, rateCmd, noteCmd)
	rootCmd.AddCommand(itemCmd)
}

// withStore opens the configured store, runs fn, and closes it.
func withStore(cmd *cobra.Command, fn func(ctx context.Context, db *store.Store) error) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(cmd.Context(), db)
}

func statusRunE(status, verb string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		link := args[0]
		return withStore(cmd, func(ctx context.Context, db *store.Store) error {
			s := status
			if err := db.UpdateUserState(ctx, link, store.UserPatch{Status: &s}); err != nil {
				return err
			}
			fmt.Printf("Marked %s: %s\n", verb, link)
			return nil
		})
	}
}

func runRate(cmd *cobra.Command, args []string) error {
	link := args[0]
	score, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("score must be an integer, got %q", args[1])
	}
	return withStore(cmd, func(ctx context.Context, db *store.Store) error {
		if err := db.UpdateUserState(ctx, link, store.UserPatch{UserScore: &score}); err != nil {
			return err
		}
		fmt.Printf("Rated %s: %d/10\n", link, score)
		return nil
	})
}

func runNote(cmd *cobra.Command, args []string) error {
	link := args[0]
	note := strings.Join(args[1:], " ")
	return withStore(cmd, func(ctx context.Context, db *store.Store) error {
		if err := db.UpdateUserState(ctx, link, store.UserPatch{UserNote: &note}); err != nil {
			return err
		}
		fmt.Printf("Noted: %s\n", link)
		return nil
	})
}