// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/DanielBlei/rabbithole/internal/profile"
)

const profileSchema = `
CREATE TABLE IF NOT EXISTS profiles (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	content    TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	CHECK (id != 'builtin-default'),
	CHECK (length(trim(name)) > 0),
	CHECK (length(trim(content)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_profiles_name ON profiles(lower(name), id);

CREATE TABLE IF NOT EXISTS profile_state (
	singleton         INTEGER PRIMARY KEY CHECK (singleton = 1),
	active_profile_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS profile_imports (
	source      TEXT PRIMARY KEY,
	profile_id  TEXT NOT NULL,
	imported_at TIMESTAMP NOT NULL
);
`

// profileSchemaPG is the Postgres twin: TIMESTAMPTZ for the timestamps. The
// CHECKs and the lower(name) index carry over as they are.
const profileSchemaPG = `
CREATE TABLE IF NOT EXISTS profiles (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	content    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	CHECK (id != 'builtin-default'),
	CHECK (length(trim(name)) > 0),
	CHECK (length(trim(content)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_profiles_name ON profiles(lower(name), id);

CREATE TABLE IF NOT EXISTS profile_state (
	singleton         INTEGER PRIMARY KEY CHECK (singleton = 1),
	active_profile_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS profile_imports (
	source      TEXT PRIMARY KEY,
	profile_id  TEXT NOT NULL,
	imported_at TIMESTAMPTZ NOT NULL
);
`

var (
	// ErrProfileNotFound is returned when a local profile ID does not exist.
	ErrProfileNotFound = errors.New("profile not found")
	// ErrProfileImmutable is returned when a store mutation targets the
	// application-owned built-in default.
	ErrProfileImmutable = errors.New("built-in profile is immutable")
)

// Profile is a mutable local interest profile. The built-in Default is not a
// Profile row and is composed by the application layer.
type Profile struct {
	ID        string
	Name      string
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateProfile validates and inserts a local profile with a stable random ID.
func (s *Store) CreateProfile(ctx context.Context, name, content string) (Profile, error) {
	id, err := newProfileID()
	if err != nil {
		return Profile{}, err
	}
	return s.createProfile(ctx, id, name, content)
}

// EnsureProfile inserts a profile under a deterministic ID if it is absent.
// Existing content is never overwritten. It is used by one-time legacy config
// bootstrap, not ordinary user creation.
func (s *Store) EnsureProfile(
	ctx context.Context,
	id, name, content string,
) (p Profile, created bool, err error) {
	if err := profile.ValidateID(id); err != nil {
		return Profile{}, false, err
	}
	name, content, err = validProfile(name, content)
	if err != nil {
		return Profile{}, false, err
	}
	now := sqlTime(time.Now())
	res, err := s.exec(ctx, `INSERT INTO profiles
		(id, name, content, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		id, name, content, now, now)
	if err != nil {
		return Profile{}, false, fmt.Errorf("ensure profile %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Profile{}, false, fmt.Errorf("profile rows affected: %w", err)
	}
	p, err = s.GetProfile(ctx, id)
	return p, n > 0, err
}

// ImportProfileOnce records a legacy bootstrap source and creates its local
// profile only on the source's first import. The marker intentionally outlives
// the profile row, so deleting an imported profile in the UI does not make it
// reappear on the next restart while config.profile still exists.
func (s *Store) ImportProfileOnce(
	ctx context.Context,
	source, id, name, content string,
) (imported bool, err error) {
	if source == "" {
		return false, fmt.Errorf("profile import source cannot be blank")
	}
	if err := profile.ValidateID(id); err != nil {
		return false, err
	}
	name, content, err = validProfile(name, content)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin profile import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := sqlTime(time.Now())
	marker, err := s.txExec(ctx, tx, `INSERT INTO profile_imports
		(source, profile_id, imported_at) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, source, id, now)
	if err != nil {
		return false, fmt.Errorf("record profile import: %w", err)
	}
	n, err := marker.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("profile import rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := s.txExec(ctx, tx, `INSERT INTO profiles
		(id, name, content, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		id, name, content, now, now); err != nil {
		return false, fmt.Errorf("import profile %s: %w", id, err)
	}
	// Preserve file-based behavior only on the first import into a database
	// that has no user selection. The profile, marker and initial activation
	// commit together.
	if _, err := s.txExec(ctx, tx, `INSERT INTO profile_state
		(singleton, active_profile_id) VALUES (1, ?)
		ON CONFLICT DO NOTHING`, id); err != nil {
		return false, fmt.Errorf("activate imported profile: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit profile import: %w", err)
	}
	return true, nil
}

func (s *Store) createProfile(ctx context.Context, id, name, content string) (Profile, error) {
	if err := profile.ValidateID(id); err != nil {
		return Profile{}, err
	}
	var err error
	name, content, err = validProfile(name, content)
	if err != nil {
		return Profile{}, err
	}
	now := sqlTime(time.Now())
	if _, err := s.exec(ctx, `INSERT INTO profiles
		(id, name, content, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, content, now, now); err != nil {
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	return s.GetProfile(ctx, id)
}

func validProfile(name, content string) (string, string, error) {
	name, err := profile.ValidateName(name)
	if err != nil {
		return "", "", err
	}
	content, err = profile.ValidateContent(content)
	if err != nil {
		return "", "", err
	}
	return name, content, nil
}

func newProfileID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate profile id: %w", err)
	}
	return "profile-" + hex.EncodeToString(raw[:]), nil
}

// ListProfiles returns local profiles ordered by display name then stable ID.
func (s *Store) ListProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.query(ctx, `SELECT id, name, content, created_at, updated_at
		FROM profiles ORDER BY lower(name), id`)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var profiles []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// GetProfile returns one local profile.
func (s *Store) GetProfile(ctx context.Context, id string) (Profile, error) {
	if id == profile.DefaultID {
		return Profile{}, fmt.Errorf("%w: %s", ErrProfileImmutable, id)
	}
	if err := profile.ValidateID(id); err != nil {
		return Profile{}, err
	}
	p, err := scanProfile(s.queryRow(ctx,
		"SELECT id, name, content, created_at, updated_at FROM profiles WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("get profile %s: %w", id, err)
	}
	return p, nil
}

func scanProfile(sc rowScanner) (Profile, error) {
	var p Profile
	err := sc.Scan(&p.ID, &p.Name, &p.Content, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// UpdateProfile changes a local profile without changing its stable ID.
func (s *Store) UpdateProfile(ctx context.Context, id, name, content string) (Profile, error) {
	if id == profile.DefaultID {
		return Profile{}, fmt.Errorf("%w: %s", ErrProfileImmutable, id)
	}
	if err := profile.ValidateID(id); err != nil {
		return Profile{}, err
	}
	name, content, err := validProfile(name, content)
	if err != nil {
		return Profile{}, err
	}
	res, err := s.exec(ctx,
		"UPDATE profiles SET name = ?, content = ?, updated_at = ? WHERE id = ?",
		name, content, sqlTime(time.Now()), id)
	if err != nil {
		return Profile{}, fmt.Errorf("update profile %s: %w", id, err)
	}
	if err := requireProfileRow(res, id); err != nil {
		return Profile{}, err
	}
	return s.GetProfile(ctx, id)
}

// DuplicateProfile copies a local profile under a new ID and display name.
func (s *Store) DuplicateProfile(ctx context.Context, id, name string) (Profile, error) {
	src, err := s.GetProfile(ctx, id)
	if err != nil {
		return Profile{}, err
	}
	return s.CreateProfile(ctx, name, src.Content)
}

// DeleteProfile removes a local profile. If it was active, the same transaction
// switches the active reference to the immutable built-in Default.
func (s *Store) DeleteProfile(ctx context.Context, id string) error {
	if id == profile.DefaultID {
		return fmt.Errorf("%w: %s", ErrProfileImmutable, id)
	}
	if err := profile.ValidateID(id); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete profile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := s.txExec(ctx, tx, "DELETE FROM profiles WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete profile %s: %w", id, err)
	}
	if err := requireProfileRow(res, id); err != nil {
		return err
	}
	if _, err := s.txExec(ctx, tx, `UPDATE profile_state
		SET active_profile_id = ? WHERE singleton = 1 AND active_profile_id = ?`,
		profile.DefaultID, id); err != nil {
		return fmt.Errorf("fall back active profile: %w", err)
	}
	return tx.Commit()
}

func requireProfileRow(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("profile rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	return nil
}

// ActiveProfileID returns the persisted active reference and whether one has
// ever been explicitly set. Absence means application policy chooses a default.
func (s *Store) ActiveProfileID(ctx context.Context) (id string, set bool, err error) {
	err = s.queryRow(ctx,
		"SELECT active_profile_id FROM profile_state WHERE singleton = 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get active profile: %w", err)
	}
	return id, true, nil
}

// SetActiveProfile persists the built-in Default or an existing local profile.
func (s *Store) SetActiveProfile(ctx context.Context, id string) error {
	if id != profile.DefaultID {
		if err := profile.ValidateID(id); err != nil {
			return err
		}
	}
	res, err := s.exec(ctx, `INSERT INTO profile_state (singleton, active_profile_id)
		SELECT 1, ? WHERE ? = ? OR EXISTS (SELECT 1 FROM profiles WHERE id = ?)
		ON CONFLICT(singleton) DO UPDATE SET active_profile_id = excluded.active_profile_id`,
		id, id, profile.DefaultID, id)
	if err != nil {
		return fmt.Errorf("set active profile: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("active profile rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	return nil
}
