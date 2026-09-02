// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package claude implements rank.Scorer against the Claude Code CLI, for use as
// an eval-time reference scorer. It is not reachable from the ingest path.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

const (
	scoreTimeout = 5 * time.Minute
	authTimeout  = 30 * time.Second
)

// binary is the CLI this backend drives.
const binary = "claude"

// instruction is the -p argument. The profile and articles go on stdin instead,
// which keeps them out of /proc/<pid>/cmdline and clear of the argv size limit.
const instruction = "Score the articles in the input above."

// stderrLimit bounds how much of a failed run's stderr reaches an error.
const stderrLimit = 4 << 10

// lookPath finds the CLI. A variable so tests need no real install.
var lookPath = exec.LookPath

// aliases are the model names accepted. Full model IDs are rejected: an alias
// list needs no model-list call and cannot be typoed into something valid.
var aliases = []string{"sonnet", "opus", "haiku", "fable"}

// Client scores items by running one `claude -p` per batch.
type Client struct {
	model        string
	systemPrompt string
	tuning       rank.ModelTuning

	// run executes a scoring call; check runs a command for its exit status
	// alone. Separate because check must not capture output.
	run   func(ctx context.Context, dir, stdin string, args []string) (stdout, stderr []byte, err error)
	check func(ctx context.Context, args []string) error

	scratchOnce sync.Once
	scratchDir  string
	scratchErr  error

	mu       sync.Mutex
	resolved string
}

// New prepares a client. model must be empty or one of aliases. systemPrompt
// replaces the CLI's own prompt when non-empty.
func New(model, systemPrompt string, tuning rank.ModelTuning) (*Client, error) {
	if _, err := lookPath(binary); err != nil {
		return nil, fmt.Errorf("%s not found on PATH: %w", binary, err)
	}
	if model != "" && !isAlias(model) {
		return nil, fmt.Errorf("model %q is not a Claude alias, use one of %s",
			model, strings.Join(aliases, ", "))
	}
	return &Client{
		model:        model,
		systemPrompt: systemPrompt,
		tuning:       tuning.Normalize(),
		run:          execRun,
		check:        execCheck,
	}, nil
}

func isAlias(m string) bool {
	for _, a := range aliases {
		if m == a {
			return true
		}
	}
	return false
}

// Validate confirms the CLI is logged in.
//
// The command's output is never captured: `claude auth status` prints the
// account email and organisation, so only its exit status is used.
func (c *Client) Validate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	if err := c.check(ctx, []string{"auth", "status"}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("claude auth status: %w", ctxErr)
		}
		return errors.New("claude CLI is not logged in; run `claude auth login`")
	}
	return nil
}

// ResolvedModel is the model the CLI actually used, e.g. "claude-sonnet-5" for
// the alias "sonnet". Empty before the first successful Score.
func (c *Client) ResolvedModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolved
}

// scoreArgs builds the argument list. Every tool is dropped and every external
// config source ignored, so an injected instruction in feed text has nothing to
// reach: the process is text in, text out.
func (c *Client) scoreArgs() []string {
	args := []string{
		"-p", instruction,
		"--output-format", "json",
		"--json-schema", c.tuning.Schema(),
		"--tools", "",
		"--restricted",
		"--permission-mode", "dontAsk",
		"--setting-sources", "",
		"--strict-mcp-config",
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if c.systemPrompt != "" {
		args = append(args, "--system-prompt", c.systemPrompt)
	}
	return args
}

// envelope is the --output-format json reply.
type envelope struct {
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	ModelUsage       map[string]struct {
		OutputTokens   int    `json:"outputTokens"`
		CanonicalModel string `json:"canonicalModel"`
	} `json:"modelUsage"`
}

// mainModel is the canonical name of the model that produced the answer. A run
// can touch more than one, so the busiest by output tokens is the scoring one.
func (e envelope) mainModel() string {
	var name string
	var most int
	for _, u := range e.ModelUsage {
		if u.CanonicalModel != "" && u.OutputTokens >= most {
			name, most = u.CanonicalModel, u.OutputTokens
		}
	}
	return name
}

// Score runs one subprocess for the batch and parses the verdicts.
func (c *Client) Score(ctx context.Context, profile string, items []feeds.Item) ([]rank.ItemScore, error) {
	logger := zerolog.Ctx(ctx)
	ctx, cancel := context.WithTimeout(ctx, scoreTimeout)
	defer cancel()

	dir, err := c.scratch()
	if err != nil {
		return nil, err
	}
	prompt := rank.BuildUserPrompt(profile, items)

	logger.Debug().Str("model", c.model).Int("items", len(items)).Msg("claude: sending scoring request")
	logger.Trace().Str("prompt", prompt).Msg("claude: prompt sent")

	start := time.Now()
	stdout, stderr, err := c.run(ctx, dir, prompt, c.scoreArgs())
	if err != nil {
		return nil, fmt.Errorf("claude -p: %w%s", err, detail(stderr))
	}

	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &env); err != nil {
		return nil, fmt.Errorf("decode claude response: %w%s", err, detail(stderr))
	}
	// A failed run can still exit 0 and report itself in the envelope.
	if env.IsError {
		return nil, fmt.Errorf("claude reported an error: %s%s",
			truncate(env.Result), detail(stderr))
	}
	if m := env.mainModel(); m != "" {
		c.mu.Lock()
		c.resolved = m
		c.mu.Unlock()
	}

	// structured_output is schema-constrained, so prefer it; result is the
	// fallback for a reply that arrived as plain text.
	raw := string(env.StructuredOutput)
	if len(env.StructuredOutput) == 0 {
		raw = env.Result
	}

	logger.Debug().
		Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
		Str("resolved_model", env.mainModel()).
		Int("response_bytes", len(raw)).
		Msg("claude: scoring response received")
	logger.Trace().Str("response", raw).Msg("claude: raw response")

	scores, err := rank.ParseScores(raw, items)
	if err != nil {
		return nil, fmt.Errorf("%w%s", err, detail(stderr))
	}
	return scores, nil
}

// scratch is a private working directory for the CLI, created once. Running
// from the repo would let the CLI pick up its CLAUDE.md and hooks.
func (c *Client) scratch() (string, error) {
	c.scratchOnce.Do(func() {
		c.scratchDir, c.scratchErr = os.MkdirTemp("", "rabbithole-claude-")
	})
	if c.scratchErr != nil {
		return "", fmt.Errorf("claude scratch dir: %w", c.scratchErr)
	}
	return c.scratchDir, nil
}

func execRun(ctx context.Context, dir, stdin string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

// execCheck runs a command for its exit status. Stdout and Stderr are left nil
// so the child writes to /dev/null and its output never enters this process.
func execCheck(ctx context.Context, args []string) error {
	return exec.CommandContext(ctx, binary, args...).Run()
}

// detail renders captured stderr for an error message, or nothing.
func detail(stderr []byte) string {
	s := strings.TrimSpace(string(stderr))
	if s == "" {
		return ""
	}
	return ": " + truncate(s)
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > stderrLimit {
		return s[:stderrLimit] + "..."
	}
	return s
}
