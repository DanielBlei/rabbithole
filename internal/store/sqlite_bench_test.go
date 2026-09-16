// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"fmt"
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
