// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func BenchmarkListByScore(b *testing.B) {
	db := openTestStore(b)
	ctx := context.Background()
	items := make([]feeds.Item, 200)
	scored := make([]DigestEntry, 200)
	for i := range items {
		items[i] = feeds.Item{
			ID:        fmt.Sprintf("id-%03d", i),
			Source:    fmt.Sprintf("source-%d", i%8),
			Title:     fmt.Sprintf("Paper %03d", i),
			Link:      fmt.Sprintf("https://example.test/%d", i),
			Published: time.Now().Add(-time.Duration(i) * time.Hour),
		}
		scored[i] = DigestEntry{Item: items[i], Score: i % 11, Reason: "benchmark"}
	}
	if err := db.Record(ctx, items, scored, time.Now()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := db.List(ctx, ListFilter{Limit: 50}); err != nil {
			b.Fatal(err)
		}
	}
}

// seedSourceStats inserts n items over 90 days across sources, with random
// scores, status, ratings and bookmarks, in one transaction.
func seedSourceStats(b *testing.B, db *Store, sources, n int) {
	b.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 2))
	end := time.Now()
	err := db.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := db.prepareTx(ctx, tx, `INSERT INTO items
			(id, source, title, link, created_at, updated_at, llm_score, status, user_score, bookmarked)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		statuses := []string{StatusUnread, StatusRead, StatusSkipped}
		for i := 0; i < n; i++ {
			at := sqlTime(end.Add(-time.Duration(rng.Int64N(int64(90 * 24 * time.Hour)))))
			var score, rating any
			if rng.IntN(10) > 0 {
				score = rng.IntN(11)
			}
			if rng.IntN(20) == 0 {
				rating = 10 * rng.IntN(2)
			}
			if _, err := stmt.ExecContext(ctx, fmt.Sprintf("id-%d", i), fmt.Sprintf("source-%d", i%sources),
				"t", fmt.Sprintf("https://example.test/%d", i), at, at, score,
				statuses[rng.IntN(len(statuses))], rating, rng.IntN(25) == 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

func BenchmarkSourceStats(b *testing.B) {
	db := openTestStore(b)
	seedSourceStats(b, db, 60, 50000)
	ctx := context.Background()
	now := time.Now()
	opts := SourceStatsOpts{High: 7, Mid: 4, ThumbUp: 10, ThumbDown: 0}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := db.SourceStats(ctx, now, opts); err != nil {
			b.Fatal(err)
		}
	}
}
