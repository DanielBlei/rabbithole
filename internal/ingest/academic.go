// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// fetchAcademic is a placeholder: academic ingest isn't implemented yet, so
// every academic feed is logged and skipped rather than fetched.
func fetchAcademic(ctx context.Context, sources []feeds.Source) []feeds.Result {
	return skipUnimplemented(ctx, sources, config.FeedTypeAcademic)
}
