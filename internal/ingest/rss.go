// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// fetchRSS fetches RSS/Atom sources. It is the only feed type ingest actually
// implements today; the feeds package does the parsing.
func fetchRSS(ctx context.Context, sources []feeds.Source) []feeds.Result {
	return feeds.FetchAll(ctx, sources)
}
