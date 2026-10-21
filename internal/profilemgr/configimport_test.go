// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package profilemgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
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
	a, _ := config.ParseFile([]byte("inference:\n  provider: ollama\n"))
	b, _ := config.ParseFile([]byte("inference:\n  provider: ollama\n  think: true\n"))
	pa, err := Prepare(a, "a", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	pb, err := Prepare(b, "b", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if pa.Hash == pb.Hash || pa.EffectiveHash != pb.EffectiveHash {
		t.Errorf("hash %v/%v effective %v/%v: want different hashes, equal effective hashes",
			pa.Hash == pb.Hash, "", pa.EffectiveHash, pb.EffectiveHash)
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
