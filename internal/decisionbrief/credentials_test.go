// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package decisionbrief

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/articletext"
	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Additional D: credentials in a feed link or a fetcher's final URL must not
// reach the model prompt, a log line, or the persisted brief.
func TestCredentialedURLsNeverReachPromptLogOrStore(t *testing.T) {
	const (
		feedSecret  = "feed-s3cr3t"
		fetchSecret = "fetch-s3cr3t"
	)
	db := testStore(t, feeds.Item{
		ID: "cred", Source: "S", Title: "Credentialed",
		Link: "https://feeduser:" + feedSecret + "@example.com/cred",
	})
	var logs bytes.Buffer
	ctx := zerolog.New(&logs).WithContext(context.Background())

	var prompt string
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(_ context.Context, _, user string, _ json.RawMessage, _ int) (string, error) {
			prompt = user
			return validJSON, nil
		}), Provenance{Provider: "test", Model: "test"}, nil
	})
	svc.SetFetcher(func(context.Context, string, articlefetch.Limits) (articlefetch.Result, error) {
		return articlefetch.Result{
			Body:        io.NopCloser(strings.NewReader(validBody())),
			ContentType: "text/html; charset=utf-8",
			FinalURL:    "https://fetchuser:" + fetchSecret + "@example.com/final",
		}, nil
	})

	if _, err := svc.Start(ctx, "cred", false, Options{MaxTextBytes: 900}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "cred")
	if snap.Status != StatusReady {
		t.Fatalf("status = %s (%s: %s)", snap.Status, snap.ErrorCode, snap.ErrorDetail)
	}
	if got, want := snap.Brief.FinalURL, "https://example.com/final"; got != want {
		t.Errorf("stored FinalURL = %q, want %q", got, want)
	}
	for name, text := range map[string]string{
		"prompt": prompt,
		"log":    logs.String(),
		"stored": snap.Brief.FinalURL,
	} {
		for _, secret := range []string{feedSecret, fetchSecret, "feeduser", "fetchuser"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaked %q: %s", name, secret, text)
			}
		}
	}
	if !strings.Contains(prompt, "https://example.com/final") {
		t.Errorf("prompt lost the sanitized final link: %s", prompt)
	}
}

// The prompt's fallback link comes from the stored item when no fetch URL is
// available, and that stored link can carry userinfo too.
func TestBuildUserPromptSanitizesFallbackLink(t *testing.T) {
	const secret = "fallback-s3cr3t"
	prepared := articletext.Prepared{Text: strings.Repeat("A sentence of article prose. ", 20)}
	got := BuildUserPrompt("profile", store.ItemRow{
		Source: "S", Title: "T",
		Link: "https://fallbackuser:" + secret + "@example.com/a",
	}, prepared, "", nil)
	if strings.Contains(got, secret) || strings.Contains(got, "fallbackuser") {
		t.Fatalf("prompt leaked fallback credentials: %s", got)
	}
	if !strings.Contains(got, "Final link: https://example.com/a") {
		t.Fatalf("prompt missing sanitized link: %s", got)
	}
}
