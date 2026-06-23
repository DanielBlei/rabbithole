// Render and Write turn ranked results into a dated markdown digest file.
package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DanielBlei/ai-searcher/internal/rank"
)

// Render builds the markdown document for a day's digest.
func Render(day time.Time, results []rank.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Reading digest — %s\n\n", day.Format("Monday, 2 January 2006"))
	if len(results) == 0 {
		b.WriteString("_No new items cleared the relevance threshold today._\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d item(s) worth your time, best first.\n\n", len(results))
	for _, r := range results {
		fmt.Fprintf(&b, "## [%d/10] %s\n\n", r.Score, r.Item.Title)
		if r.Reason != "" {
			fmt.Fprintf(&b, "> %s\n\n", r.Reason)
		}
		meta := r.Item.Source
		if !r.Item.Published.IsZero() {
			meta += " · " + r.Item.Published.Format("2 Jan 2006")
		}
		fmt.Fprintf(&b, "%s\n", meta)
		fmt.Fprintf(&b, "<%s>\n\n", r.Item.Link)
	}
	return b.String()
}

// Write renders the digest and writes it to dir/YYYY-MM-DD.md, returning the path.
func Write(dir string, day time.Time, results []rank.Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	path := filepath.Join(dir, day.Format("2006-01-02")+".md")
	if err := os.WriteFile(path, []byte(Render(day, results)), 0o644); err != nil {
		return "", fmt.Errorf("write digest: %w", err)
	}
	return path, nil
}
