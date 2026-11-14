// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package decisionbrief

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/articleextract"
	"github.com/DanielBlei/rabbithole/internal/articlefetch"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Finding #1: HTML beyond the extractor's supported input becomes a stable
// too_large failure; no partial or misleading ready brief is written.
func TestExtractionInputTooLargePersistsNoBrief(t *testing.T) {
	db := testStore(t, item("a"))
	body := strings.Repeat("a", articleextract.MaxInputBytes+1)
	var chatCalls int
	svc := New(db, testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(context.Context, string, string, json.RawMessage, int) (string, error) {
			chatCalls++
			return validJSON, nil
		}), Provenance{Provider: "test", Model: "test"}, nil
	})
	svc.SetFetcher(stubFetcher(body, nil))

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := waitTerminal(t, svc, "a")
	if snap.Status != StatusFailed || snap.ErrorCode != CodeTooLarge {
		t.Fatalf("snapshot = %+v, want failed/%s", snap, CodeTooLarge)
	}
	if chatCalls != 0 {
		t.Errorf("model calls = %d, want 0 after an input-size failure", chatCalls)
	}
	stored, err := db.ItemBrief(context.Background(), "a")
	if err != nil {
		t.Fatalf("stored brief: %v", err)
	}
	if stored.Status != store.ItemBriefFailed || stored.ErrorCode != CodeTooLarge {
		t.Fatalf("stored brief = %+v, want a failed/%s row", stored, CodeTooLarge)
	}
	if stored.Status == store.ItemBriefReady {
		t.Fatal("a misleading ready brief was persisted")
	}
}

// Finding #4: the declared charset travels from the fetcher through extraction
// into the prompt the model sees.
func TestBriefPromptUsesDeclaredCharset(t *testing.T) {
	body := "<html><body><article><p>caf\xe9 tr\xe8s bon. " +
		strings.Repeat("Enough words to pass the minimum content guard. ", 20) +
		"</p></article></body></html>"
	var prompt string
	svc := New(testStore(t, item("a")), testProfile(), func(context.Context) (Generator, Provenance, error) {
		return genFunc(func(_ context.Context, _, user string, _ json.RawMessage, _ int) (string, error) {
			prompt = user
			return validJSON, nil
		}), Provenance{Provider: "test", Model: "test"}, nil
	})
	svc.SetFetcher(func(_ context.Context, rawURL string, _ articlefetch.Limits) (articlefetch.Result, error) {
		return articlefetch.Result{
			Body:        io.NopCloser(strings.NewReader(body)),
			ContentType: "text/html; charset=iso-8859-1",
			FinalURL:    rawURL,
		}, nil
	})

	if _, err := svc.Start(context.Background(), "a", false, Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snap := waitTerminal(t, svc, "a"); snap.Status != StatusReady {
		t.Fatalf("status = %+v, want ready", snap)
	}
	if !strings.Contains(prompt, "café très bon") {
		t.Errorf("prompt did not receive decoded article text: %q", prompt)
	}
}
