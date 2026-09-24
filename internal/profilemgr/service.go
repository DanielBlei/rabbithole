// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package profilemgr composes the application-owned built-in profile with
// mutable local profiles persisted by store.
package profilemgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// Item is one profile as shown by application and web layers. Content is the
// stored raw Markdown; Snapshot returns the comment-stripped scoring input.
type Item struct {
	ID        string
	Name      string
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
	Builtin   bool
	Active    bool
}

// Snapshot returns the exact immutable value to use for a scoring run.
func (p Item) Snapshot() profile.Snapshot {
	return profile.NewSnapshot(p.ID, p.Name, p.Content, p.Builtin)
}

// Service resolves and mutates profiles without exposing SQL to callers.
type Service struct {
	db *store.Store
}

// New returns a profile service over db.
func New(db *store.Store) *Service { return &Service{db: db} }

// BootstrapLegacy validates and imports config.profile once. ImportProfileOnce
// transactionally activates the deterministic import only when no active
// reference has ever been persisted, preserving legacy behavior without
// overwriting a later web selection on every restart.
func (s *Service) BootstrapLegacy(ctx context.Context, cfg *config.Config) error {
	if cfg.Profile == "" {
		return nil
	}
	content, err := cfg.LoadProfile()
	if err != nil {
		return err
	}
	path, err := filepath.Abs(cfg.Profile)
	if err != nil {
		return fmt.Errorf("resolve profile path %q: %w", cfg.Profile, err)
	}
	id := legacyID(filepath.Clean(path))
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if name == "" || name == "." {
		name = "Imported profile"
	} else {
		name += " (imported)"
	}
	_, err = s.db.ImportProfileOnce(ctx, path, id, name, content)
	if err != nil {
		return fmt.Errorf("import configured profile: %w", err)
	}
	return nil
}

func legacyID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "legacy-" + hex.EncodeToString(sum[:12])
}

// Resolve snapshots the effective active profile. No persisted state means the
// built-in Default. A stale reference is repaired to Default defensively.
func (s *Service) Resolve(ctx context.Context) (profile.Snapshot, error) {
	id, set, err := s.db.ActiveProfileID(ctx)
	if err != nil {
		return profile.Snapshot{}, err
	}
	if !set || id == profile.DefaultID {
		return profile.Default(), nil
	}
	p, err := s.db.GetProfile(ctx, id)
	if errors.Is(err, store.ErrProfileNotFound) {
		if setErr := s.db.SetActiveProfile(ctx, profile.DefaultID); setErr != nil {
			return profile.Snapshot{}, fmt.Errorf("repair missing active profile: %w", setErr)
		}
		return profile.Default(), nil
	}
	if err != nil {
		return profile.Snapshot{}, err
	}
	return localItem(p, true).Snapshot(), nil
}

// List returns the built-in Default first, followed by local profiles.
func (s *Service) List(ctx context.Context) ([]Item, error) {
	active, set, err := s.db.ActiveProfileID(ctx)
	if err != nil {
		return nil, err
	}
	if !set {
		active = profile.DefaultID
	}
	locals, err := s.db.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(locals)+1)
	def := profile.Default()
	out = append(out, Item{
		ID: def.ID, Name: def.Name, Content: def.Content, Builtin: true, Active: active == def.ID,
	})
	for _, p := range locals {
		out = append(out, localItem(p, p.ID == active))
	}
	return out, nil
}

// Get returns the built-in or local profile identified by id.
func (s *Service) Get(ctx context.Context, id string) (Item, error) {
	active, set, err := s.db.ActiveProfileID(ctx)
	if err != nil {
		return Item{}, err
	}
	if !set {
		active = profile.DefaultID
	}
	if id == profile.DefaultID {
		def := profile.Default()
		return Item{
			ID: def.ID, Name: def.Name, Content: def.Content, Builtin: true, Active: active == def.ID,
		}, nil
	}
	p, err := s.db.GetProfile(ctx, id)
	if err != nil {
		return Item{}, err
	}
	return localItem(p, p.ID == active), nil
}

func localItem(p store.Profile, active bool) Item {
	return Item{
		ID: p.ID, Name: p.Name, Content: p.Content,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Active: active,
	}
}

// Create adds a local profile.
func (s *Service) Create(ctx context.Context, name, content string) (Item, error) {
	p, err := s.db.CreateProfile(ctx, name, content)
	return localItem(p, false), err
}

// Update changes a local profile. Default is rejected by the store invariant.
func (s *Service) Update(ctx context.Context, id, name, content string) (Item, error) {
	p, err := s.db.UpdateProfile(ctx, id, name, content)
	if err != nil {
		return Item{}, err
	}
	active, _, activeErr := s.db.ActiveProfileID(ctx)
	if activeErr != nil {
		return Item{}, activeErr
	}
	return localItem(p, p.ID == active), nil
}

// Duplicate copies any profile, including the built-in Default.
func (s *Service) Duplicate(ctx context.Context, id, name string) (Item, error) {
	src, err := s.Get(ctx, id)
	if err != nil {
		return Item{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = src.Name + " copy"
	}
	return s.Create(ctx, name, src.Content)
}

// Delete removes a local profile; deleting the active one falls back to Default.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.db.DeleteProfile(ctx, id)
}

// SetActive selects Default or an existing local profile.
func (s *Service) SetActive(ctx context.Context, id string) error {
	return s.db.SetActiveProfile(ctx, id)
}
