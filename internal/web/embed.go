// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package web serves The Rabbit Hole's HTML frontend: the digest "triage" page,
// rendered server-side from the same internal/store the JSON API reads. Its
// templates and static assets are embedded via go:embed (this file), so the
// whole UI ships inside the single Go binary. internal/server mounts it.
package web

import "embed"

// templatesFS holds the layout/page/partial templates; staticFS holds the CSS,
// htmx, and the page scripts under static/js. Both are embedded relative to
// this package dir.
//
//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS
