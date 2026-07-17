package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var itemsCmd = &cobra.Command{
	Use:   "items",
	Short: "Browse and record your own read/skip/rating/notes for digest items, by id or link",
}

var (
	listStatus     string
	listSource     string
	listLimit      int
	listSince      string
	listBefore     string
	listSort       string
	listBookmarked bool
)

func init() {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List items, best score first",
		Args:  cobra.NoArgs,
		RunE:  runList,
	}
	listCmd.Flags().StringVar(&listStatus, "status", "", "filter by status (unread|read|skipped)")
	listCmd.Flags().StringVar(&listSource, "source", "", "filter by source name")
	listCmd.Flags().IntVar(&listLimit, "limit", 50, "max items to show")
	listCmd.Flags().
		StringVar(&listSince, "since", "", "only items recorded within this long ago, e.g. 3d, 12h (default: 3d)")
	listCmd.Flags().
		StringVar(&listBefore, "before", "", "only items recorded earlier than this long ago, e.g. 3d (default: unbounded)")
	listCmd.Flags().
		StringVar(&listSort, "sort", "", "sort order: score (default, best first), latest (newest first), or oldest (oldest first)")
	listCmd.Flags().BoolVar(&listBookmarked, "bookmarked", false, "show only bookmarked items")

	sourcesCmd := &cobra.Command{
		Use:   "sources",
		Short: "List sources with item counts",
		Args:  cobra.NoArgs,
		RunE:  runSources,
	}

	readCmd := &cobra.Command{
		Use:   "read <id|link>...",
		Short: "Mark item(s) as read",
		Args:  cobra.MinimumNArgs(1),
		RunE:  statusRunE(store.StatusRead, "read"),
	}
	skipCmd := &cobra.Command{
		Use:   "skip <id|link>...",
		Short: "Mark item(s) as skipped",
		Args:  cobra.MinimumNArgs(1),
		RunE:  statusRunE(store.StatusSkipped, "skipped"),
	}
	unreadCmd := &cobra.Command{
		Use:   "unread <id|link>...",
		Short: "Reset item(s) back to unread",
		Args:  cobra.MinimumNArgs(1),
		RunE:  statusRunE(store.StatusUnread, "unread"),
	}
	rateCmd := &cobra.Command{
		Use:   "rate <id|link> <0-10>",
		Short: "Give an item your own relevance score",
		Args:  cobra.ExactArgs(2),
		RunE:  runRate,
	}
	noteCmd := &cobra.Command{
		Use:   "note <id|link> <text>...",
		Short: "Attach a free-text note to an item",
		Args:  cobra.MinimumNArgs(2),
		RunE:  runNote,
	}
	bookmarkCmd := &cobra.Command{
		Use:   "bookmark <id|link>...",
		Short: "Bookmark item(s) to revisit later",
		Args:  cobra.MinimumNArgs(1),
		RunE:  bookmarkRunE(true, "bookmarked"),
	}
	unbookmarkCmd := &cobra.Command{
		Use:   "unbookmark <id|link>...",
		Short: "Remove item(s) from bookmarks",
		Args:  cobra.MinimumNArgs(1),
		RunE:  bookmarkRunE(false, "unbookmarked"),
	}
	itemsCmd.AddCommand(listCmd, sourcesCmd, readCmd, skipCmd, unreadCmd, rateCmd, noteCmd, bookmarkCmd, unbookmarkCmd)
	rootCmd.AddCommand(itemsCmd)
}

// withStore opens the configured store, runs fn, and closes it. The loaded
// config is passed through so callers can apply config-driven defaults (e.g.
// `items list`'s default window) without reloading it.
func withStore(cmd *cobra.Command, fn func(ctx context.Context, db *store.Store, cfg *config.Config) error) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.Store.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn().Err(err).Msg("db close failed")
		}
	}()
	return fn(cmd.Context(), db, cfg)
}

// applyToEach runs fn for every identifier, continuing past per-item errors
// (mirroring `kubectl delete`'s behavior for multiple resource names) and
// printing each failure to stderr as it happens. Once every identifier has
// been tried, it returns a non-nil error summarizing how many failed.
func applyToEach(identifiers []string, fn func(string) error) error {
	var failed int
	for _, id := range identifiers {
		if err := fn(id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", id, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d item(s) failed", failed, len(identifiers))
	}
	return nil
}

func statusRunE(status, verb string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
			return applyToEach(args, func(identifier string) error {
				s := status
				if err := db.UpdateUserState(ctx, identifier, store.UserPatch{Status: &s}); err != nil {
					return err
				}
				fmt.Printf("Marked %s: %s\n", verb, identifier)
				return nil
			})
		})
	}
}

func bookmarkRunE(value bool, verb string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
			return applyToEach(args, func(identifier string) error {
				v := value
				if err := db.UpdateUserState(ctx, identifier, store.UserPatch{Bookmarked: &v}); err != nil {
					return err
				}
				fmt.Printf("Marked %s: %s\n", verb, identifier)
				return nil
			})
		})
	}
}

func runRate(cmd *cobra.Command, args []string) error {
	identifier := args[0]
	score, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("score must be an integer, got %q", args[1])
	}
	return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
		if err := db.UpdateUserState(ctx, identifier, store.UserPatch{UserScore: &score}); err != nil {
			return err
		}
		fmt.Printf("Rated %s: %d/10\n", identifier, score)
		return nil
	})
}

func runNote(cmd *cobra.Command, args []string) error {
	identifier := args[0]
	note := strings.Join(args[1:], " ")
	return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
		if err := db.UpdateUserState(ctx, identifier, store.UserPatch{UserNote: &note}); err != nil {
			return err
		}
		fmt.Printf("Noted: %s\n", identifier)
		return nil
	})
}

// listTitleWidth caps the TITLE column in `items list` so a long headline
// doesn't push id/link off a normal terminal width.
const listTitleWidth = 50

// defaultListSince is the window a bare `items list` (no --since/--before)
// shows: a recent slice rather than the whole history.
const defaultListSince = 3 * 24 * time.Hour

// resolveListFilter turns the `items list` flags into a store.ListFilter,
// converting --since/--before (durations relative to now) into the absolute
// After/Before timestamps List expects. defaultSince is the default lookback
// for a bare `items list`; an explicit --since overrides it, and --before pages
// older without re-imposing the default.
func resolveListFilter(defaultSince time.Duration) (store.ListFilter, error) {
	filter := store.ListFilter{
		Status:     listStatus,
		Source:     listSource,
		Limit:      listLimit,
		SortBy:     listSort,
		Bookmarked: listBookmarked,
	}
	now := time.Now()

	if listSince != "" {
		d, err := config.ParseDuration(listSince)
		if err != nil {
			return store.ListFilter{}, fmt.Errorf("--since: %w", err)
		}
		filter.After = now.Add(-d)
	} else if listBefore == "" {
		// No --since and no --before: fall back to the default recent window.
		filter.After = now.Add(-defaultSince)
	}

	if listBefore != "" {
		d, err := config.ParseDuration(listBefore)
		if err != nil {
			return store.ListFilter{}, fmt.Errorf("--before: %w", err)
		}
		filter.Before = now.Add(-d)
	}
	return filter, nil
}

func runList(cmd *cobra.Command, _ []string) error {
	return withStore(cmd, func(ctx context.Context, db *store.Store, cfg *config.Config) error {
		filter, err := resolveListFilter(defaultListSince)
		if err != nil {
			return err
		}
		rows, err := db.List(ctx, filter)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("No items.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "SCORE\tSTATUS\tMARK\tSOURCE\tTITLE\tID\tLINK"); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				scoreCell(r), r.Status, bookmarkCell(r), r.Source,
				truncate(r.Title, listTitleWidth), r.ID, r.Link); err != nil {
				return err
			}
		}
		return w.Flush()
	})
}

func runSources(cmd *cobra.Command, _ []string) error {
	return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
		counts, err := db.Sources(ctx)
		if err != nil {
			return err
		}
		if len(counts) == 0 {
			fmt.Println("No sources.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "SOURCE\tCOUNT"); err != nil {
			return err
		}
		for _, c := range counts {
			if _, err := fmt.Fprintf(w, "%s\t%d\n", c.Source, c.Count); err != nil {
				return err
			}
		}
		return w.Flush()
	})
}

// scoreCell renders an item's best-available score (user_score if set, else
// llm_score), matching the ordering List uses.
func scoreCell(r store.ItemRow) string {
	if r.UserScore != nil {
		return strconv.Itoa(*r.UserScore)
	}
	if r.LLMScore != nil {
		return strconv.Itoa(*r.LLMScore)
	}
	return "-"
}

// bookmarkCell renders a star for a bookmarked item, empty otherwise, for the
// MARK column in `items list`.
func bookmarkCell(r store.ItemRow) string {
	if r.Bookmarked {
		return "★"
	}
	return ""
}

// truncate shortens s to at most n runes, appending "…" when it does.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
