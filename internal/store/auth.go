// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// authSchema holds the web UI's single login. One row at most: the CHECK pins
// the id, so id 1 is the only row there can be.
//
// No row is a state of its own, AuthInitial: a fresh install, which nobody has
// claimed yet and which serves only the page that claims it. pass_hash is NULL
// once the gate is switched off. gen is a random value rewritten on every write: the web layer
// stamps it on each session, so any credential or mode change retires every
// session issued before it, even when the write comes from another process.
// signing_key signs the "stay signed in" cookies that outlive a restart; a new
// password gets a new key, so old cookies stop verifying along with old gens.
//
// Plain TEXT and ON CONFLICT upserts only, so the table ports to Postgres as is.
const authSchema = `
CREATE TABLE IF NOT EXISTS auth (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	username   TEXT NOT NULL,
	pass_hash  TEXT,
	mode       TEXT NOT NULL CHECK (mode IN ('enabled', 'disabled')),
	gen         TEXT NOT NULL,
	signing_key TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
`

// Identical but for the CHECK quoting rules, which both engines share; kept
// beside its twin so the pair stays visible when either changes.
const authSchemaPG = `
CREATE TABLE IF NOT EXISTS auth (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	username    TEXT NOT NULL,
	pass_hash   TEXT,
	mode        TEXT NOT NULL CHECK (mode IN ('enabled', 'disabled')),
	gen         TEXT NOT NULL,
	signing_key TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
`

// AuthMode is where the login gate stands.
type AuthMode string

const (
	// AuthInitial is a fresh install: nobody has claimed it yet, so every
	// request lands on the setup page and no login is accepted.
	AuthInitial AuthMode = "initial"
	// AuthEnabled is a gate with a password of the user's own.
	AuthEnabled AuthMode = "enabled"
	// AuthDisabled is an instance its owner chose to leave open.
	AuthDisabled AuthMode = "disabled"
)

// What the username column holds on a row with no login to name: a gate that
// was switched off. The column is NOT NULL and nothing reads it back as a
// credential, so this is a placeholder rather than an account.
const noUsername = "-"

// Credential bounds. The password floor is the usual 8; the ceiling only stops
// someone making every login attempt hash a megabyte. The username floor keeps
// a slip of the keyboard from becoming the account name.
const (
	MinUsername = 3
	MaxUsername = 64
	MinPassword = 8
	MaxPassword = 256
)

var (
	// ErrBadCredentials is a failed login. It never says which half was wrong.
	ErrBadCredentials = errors.New("wrong username or password")
	// ErrInvalidAuth marks a new username or password the user can fix by
	// retyping, which the setup page shows against the form.
	ErrInvalidAuth = errors.New("invalid credentials")
	// ErrAuthChanged is a conditional write that lost to another one: the gate
	// is no longer in the state the caller read.
	ErrAuthChanged = errors.New("the login changed in the meantime")
)

// AuthState is the gate's current state, without the hash.
type AuthState struct {
	Mode     AuthMode
	Username string
	// Gen changes on every write to the auth row and is empty in AuthInitial.
	// Sessions carry the Gen they were issued under.
	Gen string
	// SigningKey signs the cookies that keep a browser signed in across a
	// restart. Empty in AuthInitial.
	SigningKey []byte
}

const (
	sqlAuthRow = `SELECT username, pass_hash, mode, gen, signing_key FROM auth WHERE id = 1`

	sqlSetPassword = `INSERT INTO auth (id, username, pass_hash, mode, gen, signing_key, updated_at)
		VALUES (1, ?, ?, 'enabled', ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET username = excluded.username, pass_hash = excluded.pass_hash,
			mode = excluded.mode, gen = excluded.gen, signing_key = excluded.signing_key,
			updated_at = excluded.updated_at`

	// Keeps the username and key already stored; a fresh install gets the
	// placeholder username and a key of its own.
	sqlDisableAuth = `INSERT INTO auth (id, username, pass_hash, mode, gen, signing_key, updated_at)
		VALUES (1, ?, NULL, 'disabled', ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET pass_hash = NULL,
			mode = excluded.mode, gen = excluded.gen, updated_at = excluded.updated_at`

	// The conditional forms: a write over AuthInitial only lands while there is
	// still no row, and one over a stored row only while its gen is the one read.
	sqlInsertAuthIfNone = `INSERT INTO auth (id, username, pass_hash, mode, gen, signing_key, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`
	sqlUpdateAuthIfGen = `UPDATE auth SET username = ?, pass_hash = ?, mode = ?, gen = ?, signing_key = ?,
			updated_at = ?
		WHERE id = 1 AND gen = ?`

	sqlRetireSessions = `UPDATE auth SET gen = ?, updated_at = ? WHERE id = 1 AND gen = ?`
)

// AuthState reads the gate's state. A store with no auth row reports
// AuthInitial, which names nobody: the instance has not been claimed.
func (s *Store) AuthState(ctx context.Context) (AuthState, error) {
	st, _, err := s.authRow(ctx)
	return st, err
}

func (s *Store) authRow(ctx context.Context) (AuthState, sql.NullString, error) {
	var (
		st   AuthState
		hash sql.NullString
		mode string
		key  string
	)
	err := s.queryRow(ctx, sqlAuthRow).Scan(&st.Username, &hash, &mode, &st.Gen, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthState{Mode: AuthInitial}, hash, nil
	}
	if err != nil {
		return AuthState{}, hash, fmt.Errorf("query auth: %w", err)
	}
	st.Mode = AuthMode(mode)
	if st.SigningKey, err = hex.DecodeString(key); err != nil {
		return AuthState{}, hash, fmt.Errorf("auth signing key: %w", err)
	}
	return st, hash, nil
}

// VerifyLogin checks a login attempt against the stored credentials and returns
// the state it checked against, or ErrBadCredentials on a mismatch. With a
// password set it always hashes, even for a wrong username, so the response
// time says nothing about which half missed. A gate that is disabled, or one
// nobody has claimed yet, has no login to check and refuses every attempt.
func (s *Store) VerifyLogin(ctx context.Context, username, password string) (AuthState, error) {
	st, hash, err := s.authRow(ctx)
	if err != nil {
		return AuthState{}, err
	}
	username = strings.TrimSpace(username)
	switch st.Mode {
	case AuthEnabled:
		passOK, err := verifyPassword(hash.String, password)
		if err != nil {
			return AuthState{}, fmt.Errorf("stored password hash: %w", err)
		}
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(st.Username)) == 1
		if userOK && passOK {
			return st, nil
		}
	}
	return AuthState{}, ErrBadCredentials
}

// SetPassword stores a new login and switches the gate on, whatever state it
// was in. It moves the generation, which retires every existing session.
func (s *Store) SetPassword(ctx context.Context, username, password string) error {
	return s.setPassword(ctx, nil, username, password)
}

// SetPasswordIf is SetPassword only while the gate is still in the state whose
// Gen was read, returning ErrAuthChanged when another write got there first.
func (s *Store) SetPasswordIf(ctx context.Context, gen, username, password string) error {
	return s.setPassword(ctx, &gen, username, password)
}

func (s *Store) setPassword(ctx context.Context, ifGen *string, username, password string) error {
	username = strings.TrimSpace(username)
	if err := validateCredentials(username, password); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	gen, err := newAuthGen()
	if err != nil {
		return err
	}
	key, err := newSigningKey()
	if err != nil {
		return err
	}
	now := sqlTime(time.Now())
	var res sql.Result
	switch {
	case ifGen == nil:
		res, err = s.exec(ctx, sqlSetPassword, username, hash, gen, key, now)
	case *ifGen == "":
		res, err = s.exec(ctx, sqlInsertAuthIfNone, username, hash, AuthEnabled, gen, key, now)
	default:
		res, err = s.exec(ctx, sqlUpdateAuthIfGen, username, hash, AuthEnabled, gen, key, now, *ifGen)
	}
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return authWritten(res)
}

// DisableAuth switches the gate off and forgets the password hash, whatever
// state it was in.
func (s *Store) DisableAuth(ctx context.Context) error {
	gen, err := newAuthGen()
	if err != nil {
		return err
	}
	key, err := newSigningKey()
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, sqlDisableAuth, noUsername, gen, key, sqlTime(time.Now())); err != nil {
		return fmt.Errorf("disable auth: %w", err)
	}
	return nil
}

// DisableAuthIf is DisableAuth only while the gate is still in the state whose
// Gen was read, returning ErrAuthChanged when another write got there first.
func (s *Store) DisableAuthIf(ctx context.Context, gen string) error {
	st, _, err := s.authRow(ctx)
	if err != nil {
		return err
	}
	next, err := newAuthGen()
	if err != nil {
		return err
	}
	key, err := newSigningKey()
	if err != nil {
		return err
	}
	now := sqlTime(time.Now())
	var res sql.Result
	if gen == "" {
		res, err = s.exec(ctx, sqlInsertAuthIfNone, noUsername, nil, AuthDisabled, next, key, now)
	} else {
		res, err = s.exec(ctx, sqlUpdateAuthIfGen, st.Username, nil, AuthDisabled, next, key, now, gen)
	}
	if err != nil {
		return fmt.Errorf("disable auth: %w", err)
	}
	return authWritten(res)
}

// RetireSessionsIf moves the generation on from gen, the one the caller
// verified, and returns the new one. Every session and every "stay signed in"
// cookie issued before stops matching; the caller issues its own again under
// the new gen. A fresh install, or a gen another write already moved on (a
// password reset), gets ErrAuthChanged and nothing is written.
func (s *Store) RetireSessionsIf(ctx context.Context, gen string) (string, error) {
	next, err := newAuthGen()
	if err != nil {
		return "", err
	}
	res, err := s.exec(ctx, sqlRetireSessions, next, sqlTime(time.Now()), gen)
	if err != nil {
		return "", fmt.Errorf("retire sessions: %w", err)
	}
	if err := authWritten(res); err != nil {
		return "", err
	}
	return next, nil
}

// authWritten turns a conditional write that matched nothing into ErrAuthChanged.
func authWritten(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth write: %w", err)
	}
	if n == 0 {
		return ErrAuthChanged
	}
	return nil
}

// newSigningKey is 256 bits for HMAC-SHA256, hex-encoded for a TEXT column.
func newSigningKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate signing key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func newAuthGen() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate auth generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func validateCredentials(username, password string) error {
	switch n := utf8.RuneCountInString(username); {
	case n == 0:
		return fmt.Errorf("%w: username is empty", ErrInvalidAuth)
	case n < MinUsername:
		return fmt.Errorf("%w: username needs at least %d characters", ErrInvalidAuth, MinUsername)
	case n > MaxUsername:
		return fmt.Errorf("%w: username is longer than %d characters", ErrInvalidAuth, MaxUsername)
	}
	switch n := utf8.RuneCountInString(password); {
	case n < MinPassword:
		return fmt.Errorf("%w: password needs at least %d characters", ErrInvalidAuth, MinPassword)
	case n > MaxPassword:
		return fmt.Errorf("%w: password is longer than %d characters", ErrInvalidAuth, MaxPassword)
	}
	return nil
}

// argon2id parameters: the OWASP baseline (19 MiB, 2 passes, 1 lane). They are
// written into every hash, so raising them later leaves older hashes readable.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// b64 is the unpadded standard alphabet the PHC string format uses.
var b64 = base64.RawStdEncoding

// hashPassword returns password as an argon2id PHC string:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches a hash from hashPassword,
// using the parameters the hash itself records.
func verifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	var (
		memory, passes uint32
		threads        uint8
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil {
		return false, fmt.Errorf("argon2 parameters %q: %w", parts[3], err)
	}
	// argon2.IDKey panics below these.
	if passes < 1 || threads < 1 {
		return false, fmt.Errorf("argon2 parameters %q out of range", parts[3])
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("argon2 salt: %w", err)
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return false, errors.New("argon2 key is malformed")
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}
