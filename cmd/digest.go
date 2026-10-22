// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/digest"
	"github.com/DanielBlei/rabbithole/internal/store"
)

var (
	digestPeriod string
	digestLimit  int
)

var digestCmd = &cobra.Command{
	Use:   "digest",
	Short: "Print a daily or weekly digest of already-scored articles",
	Args:  cobra.NoArgs,
	RunE:  runDigest,
}

func init() {
	digestCmd.Flags().StringVar(&digestPeriod, "period", "daily", "reporting period: daily or weekly")
	digestCmd.Flags().IntVar(&digestLimit, "limit", 0, "max articles (default: 10 daily, 20 weekly; maximum 50)")
	rootCmd.AddCommand(digestCmd)
}

func runDigest(cmd *cobra.Command, _ []string) error {
	start, end, limit, err := resolveDigest(digestPeriod, digestLimit, cmd.Flags().Changed("limit"), time.Now())
	if err != nil {
		return err
	}
	return withStore(cmd, func(ctx context.Context, db *store.Store, _ *config.Config) error {
		rows, err := db.DigestItems(ctx, start, end, limit)
		if err != nil {
			return err
		}
		articles := make([]digest.Article, 0, len(rows))
		for _, row := range rows {
			articles = append(articles, digest.Article{
				Item: row.Item, Score: row.Score, Reason: row.Reason, EffectiveDate: row.EffectiveDate,
			})
		}
		return writeDigest(cmd.OutOrStdout(), digestPeriod, start, end, articles)
	})
}

func resolveDigest(period string, limit int, limitSet bool, now time.Time) (time.Time, time.Time, int, error) {
	start, end, err := digest.PeriodBounds(period, now)
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}
	if !limitSet {
		limit, err = digest.DefaultLimit(period)
		if err != nil {
			return time.Time{}, time.Time{}, 0, err
		}
	}
	if limit < 1 || limit > store.MaxDigestItems {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("--limit must be between 1 and %d", store.MaxDigestItems)
	}
	return start, end, limit, nil
}

func writeDigest(w io.Writer, period string, start, end time.Time, articles []digest.Article) error {
	_, err := io.WriteString(w, digest.RenderPeriod(period, start, end, articles))
	if err != nil {
		return fmt.Errorf("write digest: %w", err)
	}
	return nil
}
