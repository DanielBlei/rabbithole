// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package profilemgr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func testService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), db
}

func TestResolveDefaultsAndTracksSelection(t *testing.T) {
	s, _ := testService(t)
	got, err := s.Resolve(t.Context())
	if err != nil || got.ID != profile.DefaultID {
		t.Fatalf("fresh Resolve = %+v, %v", got, err)
	}
	local, err := s.Create(t.Context(), "Local", "# Local")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(t.Context(), local.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Resolve(t.Context())
	if err != nil || got.ID != local.ID || got.Content != "# Local" {
		t.Fatalf("selected Resolve = %+v, %v", got, err)
	}
}

func TestBootstrapLegacyOnceDoesNotOverrideSelection(t *testing.T) {
	s, db := testService(t)
	path := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(path, []byte("<!-- note -->\n# Legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Profile: path}
	if err := s.BootstrapLegacy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	first, err := s.Resolve(t.Context())
	if err != nil || first.ID == profile.DefaultID || first.Content != "# Legacy" {
		t.Fatalf("legacy Resolve = %+v, %v", first, err)
	}
	if err := db.SetActiveProfile(t.Context(), profile.DefaultID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# Changed on disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapLegacy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(t.Context())
	if err != nil || got.ID != profile.DefaultID {
		t.Fatalf("restart overrode selection: %+v, %v", got, err)
	}
	locals, err := db.ListProfiles(t.Context())
	if err != nil || len(locals) != 1 || locals[0].Content != "# Legacy" {
		t.Fatalf("bootstrap duplicated/overwrote profiles: %+v, %v", locals, err)
	}
}

func TestDeletedLegacyImportDoesNotReappear(t *testing.T) {
	s, db := testService(t)
	path := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(path, []byte("# Legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Profile: path}
	if err := s.BootstrapLegacy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	locals, err := db.ListProfiles(t.Context())
	if err != nil || len(locals) != 1 {
		t.Fatalf("imported profiles = %+v, %v", locals, err)
	}
	if err := s.Delete(t.Context(), locals[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapLegacy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	locals, err = db.ListProfiles(t.Context())
	if err != nil || len(locals) != 0 {
		t.Fatalf("deleted import reappeared: %+v, %v", locals, err)
	}
}
