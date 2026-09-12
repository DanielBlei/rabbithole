// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

func openAuthStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func password(pw string) func() (string, error) { return func() (string, error) { return pw, nil } }

func TestAuthCommands(t *testing.T) {
	db, ctx := openAuthStore(t), context.Background()
	var out bytes.Buffer

	if err := authStatus(ctx, db, &out); err != nil || !strings.Contains(out.String(), "not set up yet") {
		t.Fatalf("status on a fresh install = %q, %v", out.String(), err)
	}

	// An unclaimed instance has no account name to keep, so a reset has to be
	// told one rather than inventing a default.
	out.Reset()
	if err := authReset(ctx, db, "", password("correct horse"), &out); err == nil {
		t.Error("reset on an unclaimed instance picked a username of its own")
	}
	if err := authReset(ctx, db, "hatter", password("correct horse"), &out); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !strings.Contains(out.String(), "password set for hatter") {
		t.Errorf("reset output = %q", out.String())
	}
	if _, err := db.VerifyLogin(ctx, "hatter", "correct horse"); err != nil {
		t.Errorf("new password rejected: %v", err)
	}
	if _, err := db.VerifyLogin(ctx, "admin", "admin"); !errors.Is(err, store.ErrBadCredentials) {
		t.Errorf("a guessable login works after a reset: %v", err)
	}

	// With an account in place, a reset keeps its name unless told otherwise.
	out.Reset()
	if err := authReset(ctx, db, "", password("battery staple"), &out); err != nil {
		t.Fatalf("reset keeping the username: %v", err)
	}
	if !strings.Contains(out.String(), "password set for hatter") {
		t.Errorf("reset kept the wrong username: %q", out.String())
	}

	out.Reset()
	if err := authReset(ctx, db, " alice ", password("battery staple"), &out); err != nil {
		t.Fatalf("reset with a username: %v", err)
	}
	out.Reset()
	if err := authStatus(ctx, db, &out); err != nil || out.String() != "login: on, user alice\n" {
		t.Errorf("status = %q, %v", out.String(), err)
	}

	if err := authReset(ctx, db, "", password("short"), &out); !errors.Is(err, store.ErrInvalidAuth) {
		t.Errorf("reset with a short password = %v, want ErrInvalidAuth", err)
	}
	readFailed := errors.New("no terminal")
	if err := authReset(
		ctx,
		db,
		"",
		func() (string, error) { return "", readFailed },
		&out,
	); !errors.Is(
		err,
		readFailed,
	) {
		t.Errorf("reset with an unreadable password = %v", err)
	}
	if _, err := db.VerifyLogin(ctx, "alice", "battery staple"); err != nil {
		t.Errorf("failed resets changed the login: %v", err)
	}

	out.Reset()
	if err := authDisable(ctx, db, &out); err != nil {
		t.Fatalf("disable: %v", err)
	}
	out.Reset()
	if err := authStatus(ctx, db, &out); err != nil || !strings.HasPrefix(out.String(), "login: off") {
		t.Errorf("status after disable = %q, %v", out.String(), err)
	}
}

func TestReadNewPasswordFromStdin(t *testing.T) {
	for in, want := range map[string]string{
		"correct horse\n":          "correct horse",
		"correct horse\r\nextra\n": "correct horse",
		"no newline":               "no newline",
		" spaces kept \n":          " spaces kept ",
	} {
		got, err := readNewPassword(strings.NewReader(in), &bytes.Buffer{}, true)
		if err != nil || got != want {
			t.Errorf("readNewPassword(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := readNewPassword(strings.NewReader("\n"), &bytes.Buffer{}, true); err == nil {
		t.Error("an empty line was taken as a password")
	}
	// Without the flag it needs a terminal, and a reader is not one.
	if _, err := readNewPassword(strings.NewReader("x\n"), &bytes.Buffer{}, false); err == nil ||
		!strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("prompting without a terminal = %v, want a pointer to --password-stdin", err)
	}
}
