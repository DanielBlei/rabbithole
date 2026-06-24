# HTTP API

`ai-searcher serve` exposes the same data and operations as the `items` CLI (see
[docs/cli.md](cli.md)) over JSON. There's no frontend yet — endpoints are meant to be
consumed directly or scripted against.

## GET /api/items

Query params (all optional):

| Param | Meaning |
|---|---|
| `status` | filter by status: `unread` \| `read` \| `skipped` |
| `source` | filter by source name |
| `sort` | `score` (default, best first), `latest` (newest first), or `oldest` (oldest first) |
| `after` | RFC3339 timestamp; only items at or after this |
| `before` | RFC3339 timestamp; only items before this |
| `limit` | max items to return |

If both `after` and `before` are omitted, the window defaults to `[now - list_since, now)`
(`list_since` from config). If only `before` is given, `after` is derived as
`before - list_since`, so paging "older" by echoing the previous response's `window.after`
back as the next request's `before` keeps every page the same width without the client
needing to know `list_since` itself. If only `after` is given, the window is open-ended.

Response:

```json
{
  "items": [
    {
      "id": "...",
      "source": "...",
      "title": "...",
      "link": "...",
      "status": "unread",
      "llm_score": 8,
      "llm_score_reason": "...",
      "user_score": null,
      "published_at": "2026-01-01T00:00:00Z"
    }
  ],
  "window": { "after": "2026-01-01T00:00:00Z", "before": "2026-01-04T00:00:00Z" }
}
```

`llm_score`, `llm_score_reason`, `user_score`, and `published_at` are omitted when unset.

## GET /api/sources

Returns distinct source names with item counts:

```json
[{ "source": "Red Hat Emerging Tech", "count": 12 }]
```

## POST /api/items/{id}/read

## POST /api/items/{id}/skip

## POST /api/items/{id}/unread

Sets the item's status. `{id}` is the item's id (not its link, unlike the CLI's
`items read|skip|unread` which accepts either). Returns `204 No Content` on success, `404` if
the id doesn't exist.