// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package digest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

func TestRenderIncludesItems(t *testing.T) {
	day := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	results := []rank.Result{
		{
			Item:   feeds.Item{Title: "Scaling vLLM", Link: "https://x.com/vllm", Source: "Medium"},
			Score:  9,
			Reason: "deep",
		},
	}
	out := Render(day, results)
	for _, want := range []string{"Scaling vLLM", "https://x.com/vllm", "[9/10]", "deep", "Medium"} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderEmpty(t *testing.T) {
	out := Render(time.Now(), nil)
	if !strings.Contains(out, "No new items") {
		t.Errorf("empty digest should note no items, got: %s", out)
	}
}

func TestWriteCreatesDatedFile(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	path, err := Write(dir, day, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Base(path) != "2026-06-16.md" {
		t.Errorf("path = %s, want dated file", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("digest file not created: %v", err)
	}
}
