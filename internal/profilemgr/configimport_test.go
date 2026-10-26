// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package profilemgr

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestPrepareLiftsLiteralKeysAndKeepsTheCanonicalForm(t *testing.T) {
	file, err := config.ParseFile(
		[]byte("inference:\n  provider: heuristic\n  host: http://x\n  api_key: sk-literal-1234\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(file, "test.yaml", zerolog.Nop())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if strings.Contains(string(p.Content), "sk-literal-1234") {
		t.Errorf("the literal reached the stored form: %s", p.Content)
	}
	if len(p.Secrets) != 0 || len(p.Dropped) == 0 {
		t.Errorf(
			"heuristic reads neither key nor host: secrets=%v dropped=%v",
			p.Secrets,
			p.Dropped,
		)
	}

	file, _ = config.ParseFile([]byte("inference:\n  api_key: sk-literal-1234\n"))
	p, err = Prepare(file, "test.yaml", zerolog.Nop())
	if err != nil || len(p.Secrets) != 1 ||
		!strings.Contains(string(p.Content), "api_key: $RABBITHOLE_INFERENCE_API_KEY") {
		t.Errorf("Prepare = %+v, %v; want the key lifted into a reference", p, err)
	}
}

func TestPrepareRefusesAnInvalidConfigAndNamesTheFile(t *testing.T) {
	file, _ := config.ParseFile([]byte("inference:\n  provider: nope\n"))
	if _, err := Prepare(
		file,
		"bad.yaml",
		zerolog.Nop(),
	); err == nil ||
		!strings.Contains(err.Error(), "bad.yaml is not a valid config") {
		t.Errorf("Prepare = %v, want a refusal naming the file", err)
	}
	if _, err := PrepareRun(config.Config{Inference: config.InferenceConfig{Provider: "nope"}}); err == nil {
		t.Error("PrepareRun accepted an invalid provider")
	}
	a, _ := PrepareRun(config.Config{User: "x"})
	b, _ := PrepareRun(config.Config{User: "x"})
	if a.Hash == "" || a.Hash != b.Hash {
		t.Error("equal configs must hash equal")
	}
}

func TestPrepareReturnsDeprecationWarningsAndTheSinceDirective(t *testing.T) {
	file, _ := config.ParseFile(
		[]byte(
			"ingest:\n  since: 3d\n  digest_dir: /tmp/d\ninference:\n  summary:\n    model: s\n",
		),
	)
	p, err := Prepare(file, "test.yaml", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 3 {
		t.Errorf("warnings = %q, want one per deprecated key", p.Warnings)
	}
	if p.Legacy.Since.Std() != 72*time.Hour {
		t.Errorf("legacy since = %s, want 3d", p.Legacy.Since)
	}
	if strings.Contains(string(p.Content), "since") ||
		strings.Contains(string(p.Content), "summary") {
		t.Errorf("stored form keeps a removed key:\n%s", p.Content)
	}

	neg, _ := config.ParseFile([]byte("ingest:\n  since: -1d\n"))
	if _, err := Prepare(
		neg,
		"neg.yaml",
		zerolog.Nop(),
	); err == nil ||
		!strings.Contains(err.Error(), "ingest.since must be positive") {
		t.Errorf("negative since error = %v", err)
	}
}

func TestPreparedEffectiveHashIgnoresPinnedDefaults(t *testing.T) {
	unset, _ := PrepareRun(config.Config{})
	pinned, err := PrepareRun(config.Config{Inference: config.InferenceConfig{Provider: "ollama"}})
	if err != nil {
		t.Fatal(err)
	}
	if unset.Hash == pinned.Hash || unset.EffectiveHash != pinned.EffectiveHash {
		t.Errorf("hash equal %v, effective %v/%v: want different hashes, equal effective hashes",
			unset.Hash == pinned.Hash, unset.EffectiveHash, pinned.EffectiveHash)
	}
}

func TestPrepareDropsValuesEqualToTheDefault(t *testing.T) {
	prompt, _ := json.Marshal(rank.DefaultSystemPrompt + "\n")
	tests := []struct {
		name, yaml  string
		want        string // the stored form
		wantDropped int
	}{
		{"nothing set", "{}\n", "{}\n", 0},
		{"pins only", "inference:\n  provider: ollama\n  think: true\n  batch_size: 1\n", "{}\n", 3},
		{"mixed keeps the change", "inference:\n  provider: ollama\n  model: other\n  think: true\n  max_parallel: 4\n",
			"inference:\n  model: other\n  max_parallel: 4\n", 2},
		{"tuning default", "inference:\n  model_tuning:\n    tokens_per_item: 1024\n    num_ctx: 8192\n",
			"inference:\n  model_tuning:\n    num_ctx: 8192\n", 1},
		{"system prompt equal to the built-in", "inference:\n  system_prompt: " + string(prompt) + "\n", "{}\n", 1},
		{
			"system prompt off is kept",
			"inference:\n  system_prompt: false\n",
			"inference:\n  system_prompt: false\n",
			0,
		},
		{"another provider keeps its own", "inference:\n  provider: openai\n  effort: medium\n  think: true\n",
			"inference:\n  provider: openai\n", 2},
		{"heuristic reads few", "inference:\n  provider: heuristic\n  batch_size: 1\n",
			"inference:\n  provider: heuristic\n", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file, err := config.ParseFile([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			p, err := Prepare(file, "t.yaml", zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			if string(p.Content) != tc.want || len(p.Defaulted) != tc.wantDropped {
				t.Errorf(
					"stored %q, defaulted %v; want %q and %d dropped",
					p.Content,
					p.Defaulted,
					tc.want,
					tc.wantDropped,
				)
			}
		})
	}
	user := config.DefaultUser()
	if user == "" {
		t.Skip("no OS login to compare the user against")
	}
	file, _ := config.ParseFile([]byte("user: " + user + "\n"))
	if p, err := Prepare(
		file,
		"t.yaml",
		zerolog.Nop(),
	); err != nil || string(p.Content) != "{}\n" ||
		len(p.Defaulted) != 1 {
		t.Errorf("user equal to the OS login: %q, %v, %v", p.Content, p.Defaulted, err)
	}
}

func TestAppliedHashCoversDirectivesAndReferencedFiles(t *testing.T) {
	dir := t.TempDir()
	feeds := filepath.Join(dir, "feeds.yaml")
	if err := os.WriteFile(feeds, []byte("feeds: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prep := func(body string) Prepared {
		t.Helper()
		file, err := config.ParseFile([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		p, err := Prepare(file, "t.yaml", zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "inference:\n  provider: ollama\n"
	withFeeds := base + "ingest:\n  feeds: " + feeds + "\n"
	a := prep(withFeeds).AppliedHash()
	if a != prep(withFeeds).AppliedHash() {
		t.Error("the hash is not deterministic")
	}
	if a == prep(base).AppliedHash() {
		t.Error("naming a feeds file did not change the hash")
	}
	if a == prep(withFeeds+"  since: 2d\n").AppliedHash() {
		t.Error("ingest.since did not change the hash")
	}
	if err := os.WriteFile(feeds, []byte("feeds:\n  - name: A\n    url: http://a.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a == prep(withFeeds).AppliedHash() {
		t.Error("editing the feeds file did not change the hash")
	}
}

func TestApplyImportUploadKeepsNoMarker(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := PrepareRun(config.Config{User: "x"})
	if err != nil {
		t.Fatal(err)
	}
	source := store.ConfigSourceImport + "upload"
	out, err := ApplyImport(t.Context(), db, nil, p, "an uploaded file", source, ImportOpts{})
	if err != nil || !out.Saved {
		t.Fatalf("upload = %+v, %v; want saved", out, err)
	}
	if _, ok, err := db.ConfigImport(t.Context(), source); err != nil || ok {
		t.Errorf("marker after an upload = ok %v, err %v; want none", ok, err)
	}
}

func TestApplyImportUploadConflictIsNotRetried(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := PrepareRun(config.Config{User: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := PrepareRun(config.Config{User: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	opts := ImportOpts{BeforeSave: func() {
		if _, err := db.SaveConfig(
			t.Context(),
			string(other.Content),
			other.Hash,
			store.ConfigSourceUI,
			0,
		); err != nil {
			t.Fatal(err)
		}
	}}
	_, err = ApplyImport(t.Context(), db, nil, p, "an uploaded file", store.ConfigSourceImport+"upload", opts)
	if !errors.Is(err, store.ErrConfigConflict) {
		t.Fatalf("err = %v, want ErrConfigConflict", err)
	}
	if v, _, _ := db.ActiveConfig(t.Context()); v.Source != store.ConfigSourceUI {
		t.Errorf("active source = %q: the concurrent save was overwritten", v.Source)
	}

	// A reviewed version that is no longer active is refused before anything is read.
	zero := 0
	_, err = ApplyImport(t.Context(), db, nil, p, "an uploaded file", store.ConfigSourceImport+"upload",
		ImportOpts{ExpectedVersion: &zero})
	if !errors.Is(err, store.ErrConfigConflict) {
		t.Errorf("stale expected version: err = %v", err)
	}
}
