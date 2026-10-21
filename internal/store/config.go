// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// configSchema holds the run config as an append-only history: one row per
// saved version, the highest being the one in force. There is no generated id.
// A save inserts expected+1, so two saves based on the same version collide on
// the primary key and the loser is told its copy is stale.
//
// content is canonical YAML kept as opaque text; source is "ui",
// "import:<path>" or "rollback:<version>". No row means the embedded defaults.
//
// Plain TEXT and INTEGER only, so the table ports to Postgres as is.
const configSchema = `
CREATE TABLE IF NOT EXISTS config_versions (
	version        INTEGER PRIMARY KEY,
	schema_version INTEGER NOT NULL,
	content        TEXT NOT NULL,
	content_hash   TEXT NOT NULL,
	source         TEXT NOT NULL,
	created_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_config_versions_hash ON config_versions(content_hash);
`

// configImportSchema records, per --config file, the hash of what was last
// applied from it. The history cannot say this: an import that matches the
// active version saves nothing, and a UI save hides an earlier import.
const configImportSchema = `
CREATE TABLE IF NOT EXISTS config_imports (
	source       TEXT PRIMARY KEY,
	applied_hash TEXT NOT NULL,
	applied_at   TEXT NOT NULL
);
`

var (
	// ErrConfigConflict means the save was based on a version that is no longer
	// the latest.
	ErrConfigConflict = errors.New("config changed since it was loaded")
	// ErrConfigVersionNotFound means no such config version exists.
	ErrConfigVersionNotFound = errors.New("config version not found")
)

// Prefixes a config version's source may carry; "ui" stands alone.
const (
	ConfigSourceUI       = "ui"
	ConfigSourceImport   = "import:"
	ConfigSourceRollback = "rollback:"
)

// ConfigVersion is one saved run config.
type ConfigVersion struct {
	Version       int
	SchemaVersion int
	Content       string
	ContentHash   string
	Source        string
	CreatedAt     time.Time
}

const configColumns = "version, schema_version, content, content_hash, source, created_at"

func scanConfigVersion(sc interface{ Scan(...any) error }) (ConfigVersion, error) {
	var (
		v       ConfigVersion
		created string
	)
	if err := sc.Scan(&v.Version, &v.SchemaVersion, &v.Content, &v.ContentHash, &v.Source, &created); err != nil {
		return ConfigVersion{}, err
	}
	t, err := time.Parse(sqlTimeLayout, created)
	if err != nil {
		return ConfigVersion{}, fmt.Errorf("parse config created_at %q: %w", created, err)
	}
	v.CreatedAt = utcTime(t)
	return v, nil
}

// ActiveConfig returns the highest version, and false when none has been saved.
func (s *Store) ActiveConfig(ctx context.Context) (ConfigVersion, bool, error) {
	v, err := scanConfigVersion(s.queryRow(ctx,
		"SELECT "+configColumns+" FROM config_versions ORDER BY version DESC LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigVersion{}, false, nil
	}
	if err != nil {
		return ConfigVersion{}, false, fmt.Errorf("read active config: %w", err)
	}
	return v, true, nil
}

// ConfigVersion returns one saved version.
func (s *Store) ConfigVersion(ctx context.Context, version int) (ConfigVersion, error) {
	v, err := scanConfigVersion(s.queryRow(ctx,
		"SELECT "+configColumns+" FROM config_versions WHERE version = ?", version))
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigVersion{}, fmt.Errorf("%w: %d", ErrConfigVersionNotFound, version)
	}
	if err != nil {
		return ConfigVersion{}, fmt.Errorf("read config version %d: %w", version, err)
	}
	return v, nil
}

// ConfigHistory lists versions newest first; limit <= 0 lists them all.
func (s *Store) ConfigHistory(ctx context.Context, limit int) ([]ConfigVersion, error) {
	q := "SELECT " + configColumns + " FROM config_versions ORDER BY version DESC"
	var args []any
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list config history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConfigVersion
	for rows.Next() {
		v, err := scanConfigVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan config version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list config history: %w", err)
	}
	return out, nil
}

// SaveConfig stores content as version expected+1. expected is the version the
// caller based its change on, 0 when nothing is saved yet. It returns
// ErrConfigConflict when expected is not the latest version.
func (s *Store) SaveConfig(ctx context.Context, content, hash, source string, expected int) (ConfigVersion, error) {
	if strings.TrimSpace(content) == "" {
		return ConfigVersion{}, errors.New("save config: content is empty")
	}
	if hash == "" {
		return ConfigVersion{}, errors.New("save config: content hash is empty")
	}
	if !validConfigSource(source) {
		return ConfigVersion{}, fmt.Errorf("save config: unknown source %q", source)
	}
	return s.appendConfig(ctx, config.RunSchemaVersion, content, hash, source, expected)
}

// RollbackConfig copies an earlier version forward as a new latest one.
func (s *Store) RollbackConfig(ctx context.Context, version, expected int) (ConfigVersion, error) {
	old, err := s.ConfigVersion(ctx, version)
	if err != nil {
		return ConfigVersion{}, err
	}
	return s.appendConfig(ctx, old.SchemaVersion, old.Content, old.ContentHash,
		fmt.Sprintf("%s%d", ConfigSourceRollback, version), expected)
}

// appendConfig inserts expected+1 only while expected is still the highest
// version, in one statement so SQLite takes its write lock up front rather than
// failing a read-then-write upgrade. On Postgres two callers can both pass the
// guard; the primary key fails the loser.
func (s *Store) appendConfig(
	ctx context.Context,
	schemaVersion int,
	content, hash, source string,
	expected int,
) (ConfigVersion, error) {
	if expected < 0 {
		return ConfigVersion{}, fmt.Errorf("save config: expected version %d is negative", expected)
	}
	now := time.Now().UTC()
	res, err := s.exec(ctx,
		`INSERT INTO config_versions (`+configColumns+`)
		 SELECT CAST(? AS INTEGER), CAST(? AS INTEGER), ?, ?, ?, ?
		 WHERE (SELECT COALESCE(MAX(version), 0) FROM config_versions) = CAST(? AS INTEGER)`,
		expected+1, schemaVersion, content, hash, source, sqlTime(now), expected)
	if err != nil {
		if s.d.uniqueViolation(err) != "" {
			return ConfigVersion{}, ErrConfigConflict
		}
		return ConfigVersion{}, fmt.Errorf("save config: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ConfigVersion{}, ErrConfigConflict
	}
	return ConfigVersion{
		Version:       expected + 1,
		SchemaVersion: schemaVersion,
		Content:       content,
		ContentHash:   hash,
		Source:        source,
		CreatedAt:     now,
	}, nil
}

// ConfigImportMarker is what was last applied from one import source.
type ConfigImportMarker struct {
	Source      string
	AppliedHash string
	AppliedAt   time.Time
}

// ConfigImport returns the marker for source, and false when none was recorded.
func (s *Store) ConfigImport(ctx context.Context, source string) (ConfigImportMarker, bool, error) {
	var (
		m       ConfigImportMarker
		applied string
	)
	err := s.queryRow(ctx,
		"SELECT source, applied_hash, applied_at FROM config_imports WHERE source = ?", source,
	).Scan(&m.Source, &m.AppliedHash, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigImportMarker{}, false, nil
	}
	if err != nil {
		return ConfigImportMarker{}, false, fmt.Errorf("read config import marker: %w", err)
	}
	t, err := time.Parse(sqlTimeLayout, applied)
	if err != nil {
		return ConfigImportMarker{}, false, fmt.Errorf("parse config import applied_at %q: %w", applied, err)
	}
	m.AppliedAt = utcTime(t)
	return m, true, nil
}

// RecordConfigImport sets the marker for source to hash.
func (s *Store) RecordConfigImport(ctx context.Context, source, hash string) error {
	if source == "" || hash == "" {
		return errors.New("record config import: source and hash are required")
	}
	_, err := s.exec(ctx,
		`INSERT INTO config_imports (source, applied_hash, applied_at) VALUES (?, ?, ?)
		 ON CONFLICT(source) DO UPDATE SET applied_hash = excluded.applied_hash, applied_at = excluded.applied_at`,
		source, hash, sqlTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("record config import: %w", err)
	}
	return nil
}

func validConfigSource(source string) bool {
	if source == ConfigSourceUI {
		return true
	}
	for _, p := range []string{ConfigSourceImport, ConfigSourceRollback} {
		if strings.HasPrefix(source, p) && len(source) > len(p) {
			return true
		}
	}
	return false
}

// ConfigSource returns the config.Source backed by this store: the active
// version, or the defaults while nothing is saved. Each call reads the latest
// version number from the table; the parsed form is cached per version, so a
// save from another process shows up on the next call.
func (s *Store) ConfigSource() config.Source { return &dbConfigSource{s: s} }

type dbConfigSource struct {
	s *Store

	mu      sync.Mutex
	version int
	cfg     config.Config
	badSeen int // the unreadable version already logged
}

// ActiveCanonicalConfig returns the active version in canonical form (only what
// was set, no defaults) and its number. With nothing saved it returns an empty
// config at version 0.
func (s *Store) ActiveCanonicalConfig(ctx context.Context) (config.Config, int, error) {
	v, ok, err := s.ActiveConfig(ctx)
	if err != nil {
		return config.Config{}, 0, err
	}
	if !ok {
		return config.Config{}, 0, nil
	}
	cfg, err := config.ParseStoredCanonical([]byte(v.Content), v.SchemaVersion)
	if err != nil {
		return config.Config{}, 0, unreadableConfig(v.Version, err)
	}
	return cfg, v.Version, nil
}

func unreadableConfig(version int, err error) error {
	return fmt.Errorf("stored config version %d cannot be read: %w; "+
		"import a fixed file with --config <file>", version, err)
}

// Active returns a private copy of the effective config and its version, 0 for
// the defaults. A stored row that no longer parses is an error, never defaults.
func (d *dbConfigSource) Active(ctx context.Context) (config.Config, int, error) {
	v, ok, err := d.s.ActiveConfig(ctx)
	if err != nil {
		return config.Config{}, 0, err
	}
	if !ok {
		return config.RunDefaults(), 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.version != v.Version {
		cfg, err := config.ParseStored([]byte(v.Content), v.SchemaVersion)
		if err != nil {
			err = unreadableConfig(v.Version, err)
			if d.badSeen != v.Version {
				d.badSeen = v.Version
				log.Error().Err(err).Msg("stored config unreadable")
			}
			return config.Config{}, 0, err
		}
		d.version, d.cfg = v.Version, cfg
	}
	return d.cfg.Clone(), d.version, nil
}
