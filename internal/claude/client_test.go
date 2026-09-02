// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package claude

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

// stubLookPath pretends the CLI is installed.
func stubLookPath(t *testing.T) {
	t.Helper()
	orig := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/claude", nil }
	t.Cleanup(func() { lookPath = orig })
}

var testItems = []feeds.Item{{ID: "a1", Source: "Blog", Title: "One"}}

func TestNewRejectsNonAliasModel(t *testing.T) {
	stubLookPath(t)
	for _, model := range []string{"claude-sonnet-5", "gpt-4", "Sonnet", "sonnet-4"} {
		if _, err := New(model, "", rank.ModelTuning{}); err == nil {
			t.Errorf("New(%q) succeeded, want an error", model)
		}
	}
	for _, model := range []string{"", "sonnet", "opus", "haiku", "fable"} {
		if _, err := New(model, "", rank.ModelTuning{}); err != nil {
			t.Errorf("New(%q) = %v, want no error", model, err)
		}
	}
}

func TestNewFailsWithoutBinary(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = orig })

	if _, err := New("", "", rank.ModelTuning{}); err == nil {
		t.Fatal("New succeeded without the binary on PATH, want an error")
	}
}

func TestScoreArgsContainment(t *testing.T) {
	stubLookPath(t)
	c, err := New("sonnet", "be terse", rank.ModelTuning{})
	if err != nil {
		t.Fatal(err)
	}
	args := c.scoreArgs()

	// Every tool dropped and every external config source ignored: an injected
	// instruction in feed text has nothing to reach.
	for _, want := range [][2]string{
		{"--tools", ""},
		{"--permission-mode", "dontAsk"},
		{"--setting-sources", ""},
		{"--model", "sonnet"},
		{"--system-prompt", "be terse"},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("args missing %s %q: %q", want[0], want[1], args)
		}
	}
	for _, flag := range []string{"--restricted", "--strict-mcp-config", "--json-schema"} {
		if !slices.Contains(args, flag) {
			t.Errorf("args missing %s: %q", flag, args)
		}
	}
	// --bare disables OAuth and demands an API key.
	if slices.Contains(args, "--bare") {
		t.Errorf("args carry --bare: %q", args)
	}
}

func TestScoreArgsOmitEmptyModelAndPrompt(t *testing.T) {
	stubLookPath(t)
	c, err := New("", "", rank.ModelTuning{})
	if err != nil {
		t.Fatal(err)
	}
	args := c.scoreArgs()
	for _, flag := range []string{"--model", "--system-prompt"} {
		if slices.Contains(args, flag) {
			t.Errorf("args carry %s when unset: %q", flag, args)
		}
	}
}

// newStub returns a client whose subprocess is replaced by fn.
func newStub(t *testing.T, fn func(stdin string, args []string) (string, string, error)) *Client {
	t.Helper()
	stubLookPath(t)
	c, err := New("sonnet", "", rank.ModelTuning{})
	if err != nil {
		t.Fatal(err)
	}
	c.run = func(_ context.Context, _, stdin string, args []string) ([]byte, []byte, error) {
		out, errOut, err := fn(stdin, args)
		return []byte(out), []byte(errOut), err
	}
	return c
}

const okEnvelope = `{"is_error":false,` +
	`"result":"{\"scores\":[{\"index\":1,\"score\":3,\"reason\":\"from result\"}]}",` +
	`"structured_output":{"scores":[{"index":1,"score":9,"reason":"from structured"}]},` +
	`"modelUsage":{"claude-haiku-4-5":{"outputTokens":14,"canonicalModel":"claude-haiku-4-5"},` +
	`"claude-sonnet-5":{"outputTokens":453,"canonicalModel":"claude-sonnet-5"}}}`

func TestScorePrefersStructuredOutput(t *testing.T) {
	c := newStub(t, func(string, []string) (string, string, error) { return okEnvelope, "", nil })

	scores, err := c.Score(context.Background(), "profile", testItems)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0].Score != 9 {
		t.Fatalf("scores = %+v, want one score of 9 from structured_output", scores)
	}
}

func TestScoreFallsBackToResult(t *testing.T) {
	const noStructured = `{"is_error":false,` +
		`"result":"{\"scores\":[{\"index\":1,\"score\":3,\"reason\":\"from result\"}]}"}`
	c := newStub(t, func(string, []string) (string, string, error) { return noStructured, "", nil })

	scores, err := c.Score(context.Background(), "profile", testItems)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0].Score != 3 {
		t.Fatalf("scores = %+v, want one score of 3 from result", scores)
	}
}

// The prompt carries the profile and the article text. On argv it would be
// world readable through /proc; it belongs on stdin.
func TestScoreSendsPromptOnStdinNotArgv(t *testing.T) {
	var gotStdin string
	var gotArgs []string
	c := newStub(t, func(stdin string, args []string) (string, string, error) {
		gotStdin, gotArgs = stdin, args
		return okEnvelope, "", nil
	})

	if _, err := c.Score(context.Background(), "SECRET-PROFILE", testItems); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotStdin, "SECRET-PROFILE") || !strings.Contains(gotStdin, "One") {
		t.Errorf("stdin = %q, want the profile and the article", gotStdin)
	}
	if strings.Contains(strings.Join(gotArgs, " "), "SECRET-PROFILE") {
		t.Errorf("args carry the profile: %q", gotArgs)
	}
}

func TestScoreRecordsResolvedModel(t *testing.T) {
	c := newStub(t, func(string, []string) (string, string, error) { return okEnvelope, "", nil })

	if got := c.ResolvedModel(); got != "" {
		t.Errorf("ResolvedModel() = %q before scoring, want empty", got)
	}
	if _, err := c.Score(context.Background(), "profile", testItems); err != nil {
		t.Fatal(err)
	}
	// A run touches more than one model; the busiest is the one that scored.
	if got := c.ResolvedModel(); got != "claude-sonnet-5" {
		t.Errorf("ResolvedModel() = %q, want claude-sonnet-5", got)
	}
}

func TestScoreSurfacesStderr(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		err    error
	}{
		// A failed run can still exit 0 and report itself in the envelope.
		{"envelope error", `{"is_error":true,"result":"quota exceeded"}`, nil},
		{"unparseable stdout", "not json", nil},
		{"no scores", `{"is_error":false,"result":"{}"}`, nil},
		{"subprocess failed", "", errors.New("exit status 1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newStub(t, func(string, []string) (string, string, error) {
				return tc.stdout, "claude: OAuth token expired", tc.err
			})
			_, err := c.Score(context.Background(), "profile", testItems)
			if err == nil {
				t.Fatal("Score succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "OAuth token expired") {
				t.Errorf("error = %v, want it to carry stderr", err)
			}
		})
	}
}

// Validate must never surface what `claude auth status` printed: it names the
// account and organisation.
func TestValidateHidesCommandOutput(t *testing.T) {
	stubLookPath(t)
	c, err := New("", "", rank.ModelTuning{})
	if err != nil {
		t.Fatal(err)
	}
	var gotArgs []string
	c.check = func(_ context.Context, args []string) error {
		gotArgs = args
		return errors.New("someone@example.com  Org: ACME Inc")
	}

	err = c.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate succeeded, want an error")
	}
	if strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "ACME") {
		t.Errorf("error leaks account details: %v", err)
	}
	if !slices.Equal(gotArgs, []string{"auth", "status"}) {
		t.Errorf("args = %q, want auth status", gotArgs)
	}
}

func TestValidateSucceeds(t *testing.T) {
	stubLookPath(t)
	c, err := New("", "", rank.ModelTuning{})
	if err != nil {
		t.Fatal(err)
	}
	c.check = func(context.Context, []string) error { return nil }

	if err := c.Validate(context.Background()); err != nil {
		t.Errorf("Validate() = %v, want no error", err)
	}
}
