// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/feeds"
)

// fetchBlog is a placeholder: blog ingest isn't implemented yet, so every blog
// feed is logged and skipped rather than fetched.
func fetchBlog(ctx context.Context, sources []feeds.Source) []feeds.Result {
	return skipUnimplemented(ctx, sources, config.FeedTypeBlog)
}
