// SPDX-License-Identifier: Apache-2.0
package rank

import (
	"fmt"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func BenchmarkBuildUserPrompt(b *testing.B) {
	for _, n := range []int{10, 50, 200} {
		items := make([]feeds.Item, n)
		for i := range items {
			items[i] = feeds.Item{
				ID:        fmt.Sprintf("id-%d", i),
				Source:    "arXiv",
				Title:     fmt.Sprintf("Paper %d on agentic coding", i),
				Summary:   "A realistic abstract about LLM agents, evaluation, retrieval, reliability, and software engineering.",
				Published: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			}
		}
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = BuildUserPrompt(
					"LLM agents, coding systems, evaluation, RAG, and reliability",
					items,
				)
			}
		})
	}
}
