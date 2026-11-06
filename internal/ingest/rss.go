// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"net/http"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// fetchRSS fetches RSS/Atom sources through the feeds package.
func fetchRSS(ctx context.Context, client *http.Client, sources []feeds.Source) []feeds.Result {
	return feeds.FetchAll(ctx, client, sources)
}
