// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthStartsInitialWithNoLogin(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	st, err := db.AuthState(ctx)
	if err != nil {
		t.Fatalf("AuthState: %v", err)
	}
	if st.Mode != AuthInitial || st.Username != "" || st.Gen != "" {
		t.Fatalf("fresh state = %+v, want initial with no username and no gen", st)
	}
	// Nobody has claimed the instance, so there is no login to accept and no
	// name to guess at: the setup page names nobody until someone types one.
	for _, c := range [][2]string{{"admin", "admin"}, {"admin", ""}, {"root", "admin"}, {"", ""}} {
		if _, err := db.VerifyLogin(ctx, c[0], c[1]); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("VerifyLogin(%q, %q) = %v, want ErrBadCredentials", c[0], c[1], err)
		}
	}
}

func TestSetPasswordReplacesTheInitialState(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	if err := db.SetPassword(ctx, "  alice  ", "correct horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	st, err := db.AuthState(ctx)
	if err != nil {
		t.Fatalf("AuthState: %v", err)
	}
	if st.Mode != AuthEnabled || st.Username != "alice" || st.Gen == "" {
		t.Fatalf("state = %+v, want enabled/alice/gen set", st)
	}
	if _, err := db.VerifyLogin(ctx, "alice", "correct horse"); err != nil {
		t.Fatalf("new login rejected: %v", err)
	}
	for _, c := range [][2]string{
		{"admin", "admin"}, {"alice", "wrong horse"}, {"bob", "correct horse"},
	} {
		if _, err := db.VerifyLogin(ctx, c[0], c[1]); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("VerifyLogin(%q, %q) = %v, want ErrBadCredentials", c[0], c[1], err)
		}
	}

	// A second write moves the generation, which is what retires old sessions.
	if err := db.SetPassword(ctx, "alice", "battery staple"); err != nil {
		t.Fatalf("SetPassword again: %v", err)
	}
	again, _ := db.AuthState(ctx)
	if again.Gen == st.Gen {
		t.Errorf("gen did not change across password writes: %q", again.Gen)
	}
}

func TestSetPasswordValidates(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	cases := map[string][2]string{
		"empty username":  {"   ", "long enough"},
		"long username":   {strings.Repeat("u", MaxUsername+1), "long enough"},
		"short password":  {"alice", strings.Repeat("p", MinPassword-1)},
		"long password":   {"alice", strings.Repeat("p", MaxPassword+1)},
		"password spaces": {"alice", "       "},
	}
	for name, c := range cases {
		if err := db.SetPassword(ctx, c[0], c[1]); !errors.Is(err, ErrInvalidAuth) {
			t.Errorf("%s: SetPassword = %v, want ErrInvalidAuth", name, err)
		}
	}
	if st, _ := db.AuthState(ctx); st.Mode != AuthInitial {
		t.Errorf("rejected writes changed the mode to %s", st.Mode)
	}
}

func TestDisableAuth(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	if err := db.SetPassword(ctx, "alice", "correct horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	before, _ := db.AuthState(ctx)
	if err := db.DisableAuth(ctx); err != nil {
		t.Fatalf("DisableAuth: %v", err)
	}
	st, _ := db.AuthState(ctx)
	if st.Mode != AuthDisabled || st.Username != "alice" || st.Gen == before.Gen {
		t.Fatalf("after disable = %+v, want disabled keeping alice, with a new gen", st)
	}
	// Nothing to log in to, and the old password is gone with the hash.
	if _, err := db.VerifyLogin(ctx, "alice", "correct horse"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("login on a disabled gate = %v, want ErrBadCredentials", err)
	}
}

// The setup page writes only over the state it read, so two of them racing
// cannot undo each other: the loser gets ErrAuthChanged and writes nothing.
func TestConditionalWritesLoseToAnEarlierWrite(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	// Two setup pages both read the fresh install; one sets a password first.
	if err := db.SetPasswordIf(ctx, "", "alice", "correct horse"); err != nil {
		t.Fatalf("first SetPasswordIf: %v", err)
	}
	if err := db.DisableAuthIf(ctx, ""); !errors.Is(err, ErrAuthChanged) {
		t.Fatalf("stale DisableAuthIf = %v, want ErrAuthChanged", err)
	}
	if err := db.SetPasswordIf(ctx, "", "mallory", "other horse"); !errors.Is(err, ErrAuthChanged) {
		t.Fatalf("stale SetPasswordIf = %v, want ErrAuthChanged", err)
	}
	st, _ := db.AuthState(ctx)
	if st.Mode != AuthEnabled || st.Username != "alice" {
		t.Fatalf("state = %+v, want alice's password to survive", st)
	}

	// Over a stored row, the condition is the gen that was read.
	if err := db.DisableAuth(ctx); err != nil {
		t.Fatalf("DisableAuth: %v", err)
	}
	open, _ := db.AuthState(ctx)
	if err := db.SetPasswordIf(ctx, st.Gen, "mallory", "other horse"); !errors.Is(err, ErrAuthChanged) {
		t.Fatalf("SetPasswordIf with an old gen = %v, want ErrAuthChanged", err)
	}
	if err := db.SetPasswordIf(ctx, open.Gen, "alice", "new horse!"); err != nil {
		t.Fatalf("SetPasswordIf with the current gen: %v", err)
	}
	if _, err := db.VerifyLogin(ctx, "alice", "new horse!"); err != nil {
		t.Errorf("new login rejected: %v", err)
	}
	locked, _ := db.AuthState(ctx)
	if err := db.DisableAuthIf(ctx, open.Gen); !errors.Is(err, ErrAuthChanged) {
		t.Fatalf("DisableAuthIf with an old gen = %v, want ErrAuthChanged", err)
	}
	if err := db.DisableAuthIf(ctx, locked.Gen); err != nil {
		t.Fatalf("DisableAuthIf with the current gen: %v", err)
	}
	if st, _ := db.AuthState(ctx); st.Mode != AuthDisabled || st.Username != "alice" {
		t.Errorf("state = %+v, want disabled keeping alice", st)
	}
}

func TestDisableAuthOnFreshInstall(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	if err := db.DisableAuthIf(ctx, ""); err != nil {
		t.Fatalf("DisableAuthIf: %v", err)
	}
	st, _ := db.AuthState(ctx)
	if st.Mode != AuthDisabled || st.Username != noUsername || st.Gen == "" {
		t.Fatalf("state = %+v, want disabled/%s with a gen", st, noUsername)
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct horse")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash = %q, want an argon2id PHC string", hash)
	}
	if other, _ := hashPassword("correct horse"); other == hash {
		t.Error("two hashes of one password match; the salt is not random")
	}
	if ok, err := verifyPassword(hash, "correct horse"); err != nil || !ok {
		t.Errorf("verify right password = %v, %v", ok, err)
	}
	if ok, err := verifyPassword(hash, "wrong horse"); err != nil || ok {
		t.Errorf("verify wrong password = %v, %v", ok, err)
	}
	for _, bad := range []string{
		"", "plain", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=19456,t=0,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=19456,t=2,p=1$!!$a2V5",
	} {
		if _, err := verifyPassword(bad, "x"); err == nil {
			t.Errorf("verifyPassword(%q) accepted a malformed hash", bad)
		}
	}
}

// A database created before the auth table existed gains it on the next open,
// without a version bump and without losing what it held.
func TestOpenAddsAuthTableToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for _, stmt := range (sqliteDialect{}).schemas() {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("create old schema: %v", err)
		}
	}
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO todos (title, created_at, updated_at)
		VALUES ('keep me', '2026-09-01T00:00:00.000000000Z', '2026-09-01T00:00:00.000000000Z')`); err != nil {
		t.Fatalf("seed todo: %v", err)
	}
	_ = raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open existing db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.SetPassword(ctx, "alice", "correct horse"); err != nil {
		t.Fatalf("SetPassword on migrated db: %v", err)
	}
	todos, err := db.ListTodos(ctx, TodoFilter{})
	if err != nil || len(todos) != 1 || todos[0].Title != "keep me" {
		t.Fatalf("todos after open = %+v, %v; want the seeded one", todos, err)
	}
}

func TestRetireSessionsAndSigningKeys(t *testing.T) {
	db, ctx := openTestStore(t), context.Background()

	if _, err := db.RetireSessionsIf(ctx, ""); !errors.Is(err, ErrAuthChanged) {
		t.Fatalf("RetireSessionsIf on a fresh install = %v, want ErrAuthChanged", err)
	}
	if err := db.SetPassword(ctx, "alice", "correct horse"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	before, _ := db.AuthState(ctx)
	if len(before.SigningKey) != 32 {
		t.Fatalf("signing key is %d bytes, want 32", len(before.SigningKey))
	}
	// A gen another write has moved on from loses and writes nothing.
	if _, err := db.RetireSessionsIf(ctx, "stale"); !errors.Is(err, ErrAuthChanged) {
		t.Fatalf("RetireSessionsIf on a stale gen = %v, want ErrAuthChanged", err)
	}
	if still, _ := db.AuthState(ctx); still.Gen != before.Gen {
		t.Fatalf("a stale retire moved the gen from %q to %q", before.Gen, still.Gen)
	}
	gen, err := db.RetireSessionsIf(ctx, before.Gen)
	if err != nil {
		t.Fatalf("RetireSessionsIf: %v", err)
	}
	after, _ := db.AuthState(ctx)
	if after.Gen != gen || gen == before.Gen {
		t.Errorf("gen = %q (returned %q), was %q; want a new one", after.Gen, gen, before.Gen)
	}
	if after.Mode != AuthEnabled || after.Username != "alice" || !bytes.Equal(after.SigningKey, before.SigningKey) {
		t.Errorf("retiring sessions changed more than the gen: %+v", after)
	}
	if _, err := db.VerifyLogin(ctx, "alice", "correct horse"); err != nil {
		t.Errorf("the password stopped working: %v", err)
	}

	// A new password brings a new key, so cookies signed with the old one fail.
	if err := db.SetPassword(ctx, "alice", "battery staple"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if again, _ := db.AuthState(ctx); bytes.Equal(again.SigningKey, after.SigningKey) {
		t.Error("a new password kept the old signing key")
	}
}
