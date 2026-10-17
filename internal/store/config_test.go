// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
)

func mustSaveConfig(t *testing.T, s *Store, content, hash, source string, expected int) ConfigVersion {
	t.Helper()
	v, err := s.SaveConfig(t.Context(), content, hash, source, expected)
	if err != nil {
		t.Fatalf("SaveConfig(%q, expected %d): %v", content, expected, err)
	}
	return v
}

func TestConfigEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	if _, ok, err := s.ActiveConfig(ctx); err != nil || ok {
		t.Errorf("ActiveConfig = ok %v, err %v; want not ok", ok, err)
	}
	if h, err := s.ConfigHistory(ctx, 0); err != nil || len(h) != 0 {
		t.Errorf("ConfigHistory = %v, %v; want empty", h, err)
	}
}

func TestConfigSaveAndRead(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	v1 := mustSaveConfig(t, s, "a: 1\n", "h1", "ui", 0)
	v2 := mustSaveConfig(t, s, "a: 2\n", "h2", "import:/tmp/c.yaml", 1)
	if v1.Version != 1 || v2.Version != 2 {
		t.Fatalf("versions = %d, %d; want 1, 2", v1.Version, v2.Version)
	}

	active, ok, err := s.ActiveConfig(ctx)
	if err != nil || !ok || active.Version != 2 || active.Content != "a: 2\n" {
		t.Fatalf("ActiveConfig = %+v, %v, %v; want version 2", active, ok, err)
	}
	hist, err := s.ConfigHistory(ctx, 0)
	if err != nil || len(hist) != 2 || hist[0].Version != 2 || hist[1].Version != 1 {
		t.Fatalf("ConfigHistory = %+v, %v; want 2 then 1", hist, err)
	}
	if hist, _ := s.ConfigHistory(ctx, 1); len(hist) != 1 || hist[0].Version != 2 {
		t.Errorf("ConfigHistory(1) = %+v; want only version 2", hist)
	}

	got, err := s.ConfigVersion(ctx, 1)
	if err != nil {
		t.Fatalf("ConfigVersion: %v", err)
	}
	if got != v1 {
		t.Errorf("round trip = %+v, want %+v", got, v1)
	}
	if _, err := s.ConfigVersion(ctx, 9); !errors.Is(err, ErrConfigVersionNotFound) {
		t.Errorf("ConfigVersion(9) = %v, want ErrConfigVersionNotFound", err)
	}
}

func TestConfigStaleExpectedConflicts(t *testing.T) {
	s := openTestStore(t)
	mustSaveConfig(t, s, "a: 1\n", "h1", "ui", 0)
	for _, expected := range []int{0, 2} {
		_, err := s.SaveConfig(t.Context(), "a: 2\n", "h2", "ui", expected)
		if !errors.Is(err, ErrConfigConflict) {
			t.Errorf("expected %d: err = %v, want ErrConfigConflict", expected, err)
		}
	}
	if active, _, _ := s.ActiveConfig(t.Context()); active.Version != 1 {
		t.Errorf("active = %d, want 1", active.Version)
	}
}

func TestConfigConcurrentSavesOneWins(t *testing.T) {
	s := openTestStore(t)
	mustSaveConfig(t, s, "a: 1\n", "h1", "ui", 0)

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.SaveConfig(t.Context(), "a: 2\n", "h2", "ui", 1)
		}()
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrConfigConflict):
			t.Errorf("loser got %v, want ErrConfigConflict", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d saves won, want 1", wins)
	}
	if hist, _ := s.ConfigHistory(t.Context(), 0); len(hist) != 2 {
		t.Errorf("history has %d versions, want 2", len(hist))
	}
}

func TestConfigRollback(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	v1 := mustSaveConfig(t, s, "a: 1\n", "h1", "ui", 0)
	mustSaveConfig(t, s, "a: 2\n", "h2", "ui", 1)

	if _, err := s.RollbackConfig(ctx, 1, 1); !errors.Is(err, ErrConfigConflict) {
		t.Errorf("stale rollback = %v, want ErrConfigConflict", err)
	}
	if _, err := s.RollbackConfig(ctx, 7, 2); !errors.Is(err, ErrConfigVersionNotFound) {
		t.Errorf("unknown rollback = %v, want ErrConfigVersionNotFound", err)
	}
	v3, err := s.RollbackConfig(ctx, 1, 2)
	if err != nil {
		t.Fatalf("RollbackConfig: %v", err)
	}
	if v3.Version != 3 || v3.Content != v1.Content || v3.ContentHash != "h1" || v3.Source != "rollback:1" {
		t.Errorf("rollback = %+v", v3)
	}
	got, err := s.ConfigVersion(ctx, 3)
	if err != nil || got != v3 {
		t.Errorf("stored rollback = %+v, %v; want %+v", got, err, v3)
	}
}

func TestConfigRejectsInvalidInput(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		name                  string
		content, hash, source string
		expected              int
	}{
		{"empty content", " \n", "h", "ui", 0},
		{"empty hash", "a: 1\n", "", "ui", 0},
		{"bad source", "a: 1\n", "h", "cli", 0},
		{"bare prefix", "a: 1\n", "h", "import:", 0},
		{"negative expected", "a: 1\n", "h", "ui", -1},
	}
	for _, c := range cases {
		if _, err := s.SaveConfig(
			t.Context(),
			c.content,
			c.hash,
			c.source,
			c.expected,
		); err == nil ||
			errors.Is(err, ErrConfigConflict) {
			t.Errorf("%s: err = %v, want a validation error", c.name, err)
		}
	}
	if _, ok, _ := s.ActiveConfig(t.Context()); ok {
		t.Error("an invalid save left a row")
	}
}

func TestConfigTableCreatedOnExistingDatabase(t *testing.T) {
	if onPostgres() {
		t.Skip("simulates an upgrade by reopening a SQLite file")
	}
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.exec(t.Context(), "DROP TABLE config_versions"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, ok, err := s.ActiveConfig(t.Context()); err != nil || ok {
		t.Errorf("ActiveConfig after upgrade = ok %v, err %v; want empty table", ok, err)
	}
	mustSaveConfig(t, s, "a: 1\n", "h1", "ui", 0)
}

func saveRun(t *testing.T, s *Store, cfg config.Config, expected int) ConfigVersion {
	t.Helper()
	if _, err := cfg.Canonical(); err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	content, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return mustSaveConfig(t, s, string(content), cfg.Hash(), ConfigSourceUI, expected)
}

func TestConfigSourceDefaultsWhenEmpty(t *testing.T) {
	s := openTestStore(t)
	cfg, version, err := s.ConfigSource().Active(t.Context())
	if err != nil || version != 0 {
		t.Fatalf("Active = version %d, err %v; want 0, nil", version, err)
	}
	if want := config.RunDefaults(); cfg.Inference.Provider != want.Inference.Provider ||
		cfg.Ingest.Since != want.Ingest.Since {
		t.Errorf("Active = %+v, want the defaults %+v", cfg, want)
	}
}

func TestConfigSourceParsesActiveVersion(t *testing.T) {
	s := openTestStore(t)
	src := s.ConfigSource()
	saveRun(t, s, config.Config{User: "ada", Inference: config.InferenceConfig{Provider: "heuristic"}}, 0)
	cfg, version, err := src.Active(t.Context())
	if err != nil || version != 1 || cfg.User != "ada" || cfg.Inference.Provider != "heuristic" {
		t.Fatalf("Active = %+v, version %d, err %v; want ada/heuristic at 1", cfg, version, err)
	}
	if cfg.Ingest.Since == 0 {
		t.Error("Since is zero; the effective config must carry the defaults")
	}
}

func TestConfigSourceCachesByVersion(t *testing.T) {
	s := openTestStore(t)
	src := s.ConfigSource().(*dbConfigSource)
	ctx := t.Context()
	saveRun(t, s, config.Config{User: "one"}, 0)
	if _, _, err := src.Active(ctx); err != nil {
		t.Fatal(err)
	}

	src.mu.Lock()
	src.cfg.User = "from-cache"
	src.mu.Unlock()
	if cfg, _, _ := src.Active(ctx); cfg.User != "from-cache" {
		t.Errorf("User = %q, want the cached parse to be reused for the same version", cfg.User)
	}

	saveRun(t, s, config.Config{User: "two"}, 1)
	cfg, version, err := src.Active(ctx)
	if err != nil || version != 2 || cfg.User != "two" {
		t.Errorf("Active = %q, version %d, err %v; want the new version picked up", cfg.User, version, err)
	}
}

func TestConfigSourceReturnsPrivateCopies(t *testing.T) {
	s := openTestStore(t)
	src := s.ConfigSource()
	saveRun(t, s, config.Config{}, 0)
	first, _, err := src.Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	*first.Inference.Think = !*first.Inference.Think
	second, _, _ := src.Active(t.Context())
	if *second.Inference.Think == *first.Inference.Think {
		t.Error("a caller's edit leaked into the cached config")
	}
}

func TestConfigSourceParseErrorIsReturned(t *testing.T) {
	s := openTestStore(t)
	mustSaveConfig(t, s, "bogus_key: 1\n", "h", ConfigSourceUI, 0)
	_, _, err := s.ConfigSource().Active(t.Context())
	if err == nil {
		t.Fatal("Active succeeded on an unparseable stored config; it must not fall back to defaults")
	}
	for _, want := range []string{"stored config version 1 cannot be read", "--config <file>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
