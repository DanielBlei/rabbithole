// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func existsOnly(paths ...string) func(string) bool {
	return func(p string) bool {
		for _, q := range paths {
			if p == q {
				return true
			}
		}
		return false
	}
}

const testHome = "/home/ada"

// fakeEnv is a storeEnv on linux with the given home, env and existing files.
func fakeEnv(home string, env map[string]string, exists ...string) storeEnv {
	return storeEnv{
		getenv: func(k string) string { return env[k] },
		home:   home,
		goos:   "linux",
		exists: existsOnly(exists...),
	}
}

// isolatedHome points the real environment at scratch dirs, so a test that
// resolves the default store never sees the developer's own.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	return home
}

func TestResolveStoreConfig(t *testing.T) {
	userDB := testHome + "/.local/share/rabbithole/rabbithole.db"
	fileStore := &config.StoreConfig{DBPath: "/srv/file.db"}
	tests := []struct {
		name       string
		in         storeInputs
		exists     []string
		wantStore  config.StoreConfig
		wantOrigin storeOrigin
		wantErr    string
	}{
		{
			name:       "default when nothing is named",
			wantStore:  config.StoreConfig{DBPath: userDB},
			wantOrigin: originDefault,
		},
		{
			name:       "flag path",
			in:         storeInputs{Flag: "/x/a.db"},
			wantStore:  config.StoreConfig{DBPath: "/x/a.db"},
			wantOrigin: originFlag,
		},
		{
			name:       "flag postgres url",
			in:         storeInputs{Flag: "postgres://u@h/db"},
			wantStore:  config.StoreConfig{URL: "postgres://u@h/db"},
			wantOrigin: originFlag,
		},
		{
			name:       "flag postgresql scheme",
			in:         storeInputs{Flag: "postgresql://u@h/db"},
			wantStore:  config.StoreConfig{URL: "postgresql://u@h/db"},
			wantOrigin: originFlag,
		},
		{
			name:       "env path",
			in:         storeInputs{Env: "/x/env.db"},
			wantStore:  config.StoreConfig{DBPath: "/x/env.db"},
			wantOrigin: originEnv,
		},
		{
			name:       "flag beats env and file",
			in:         storeInputs{Flag: "/x/flag.db", Env: "/x/env.db", File: fileStore},
			wantStore:  config.StoreConfig{DBPath: "/x/flag.db"},
			wantOrigin: originFlag,
		},
		{
			name:       "env beats file",
			in:         storeInputs{Env: "/x/env.db", File: fileStore},
			wantStore:  config.StoreConfig{DBPath: "/x/env.db"},
			wantOrigin: originEnv,
		},
		{
			name:       "file beats default",
			in:         storeInputs{File: fileStore},
			wantStore:  *fileStore,
			wantOrigin: originFile,
		},
		{
			name:       "empty file block falls to the default",
			in:         storeInputs{File: &config.StoreConfig{}},
			wantStore:  config.StoreConfig{DBPath: userDB},
			wantOrigin: originDefault,
		},
		{
			name: "pool flags beside a postgres url",
			in: storeInputs{
				Flag: "postgres://u@h/db", MaxConns: 3, ConnMaxLifetime: config.Duration(time.Minute),
			},
			wantStore: config.StoreConfig{
				URL: "postgres://u@h/db", MaxConns: 3, ConnMaxLifetime: config.Duration(time.Minute),
			},
			wantOrigin: originFlag,
		},
		{
			name:    "pool flag beside a sqlite path is refused",
			in:      storeInputs{Flag: "/x/a.db", MaxConns: 3},
			wantErr: "apply to a postgres:// store",
		},
		{
			name:    "file block with both targets is refused",
			in:      storeInputs{File: &config.StoreConfig{DBPath: "a.db", URL: "postgres://u@h/db"}},
			wantErr: "both db_path and url",
		},
		{
			name:       "guard does not apply to a named location",
			in:         storeInputs{Flag: "/x/a.db"},
			exists:     []string{legacyInstallerDB},
			wantStore:  config.StoreConfig{DBPath: "/x/a.db"},
			wantOrigin: originFlag,
		},
		{
			name:       "guard does not apply to a file's location",
			in:         storeInputs{File: fileStore},
			exists:     []string{legacyInstallerDB},
			wantStore:  *fileStore,
			wantOrigin: originFile,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, origin, _, err := resolveStoreConfig(tc.in, fakeEnv(testHome, nil, tc.exists...))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.wantStore || origin != tc.wantOrigin {
				t.Errorf("got %+v from %s, want %+v from %s", got, origin, tc.wantStore, tc.wantOrigin)
			}
		})
	}
}

// bootFlags sets the bootstrap globals for one test and restores them after.
func bootFlags(t *testing.T, cfg, db string) {
	t.Helper()
	oldCfg, oldDB, oldConns, oldLife := configPath, dbFlag, dbMaxConns, dbLifetime
	configPath, dbFlag, dbMaxConns, dbLifetime = cfg, db, 0, ""
	t.Cleanup(func() { configPath, dbFlag, dbMaxConns, dbLifetime = oldCfg, oldDB, oldConns, oldLife })
	t.Setenv(dbEnv, "")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveBootReadsOnlyNamedFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	env := fakeEnv(testHome, nil)
	userDB := testHome + "/.local/share/rabbithole/rabbithole.db"
	writeFile(t, "configs/config.yaml", "store:\n  db_path: ./legacy.db\n")
	writeFile(t, "rabbithole/config.yaml", "store:\n  db_path: ./other.db\n")

	t.Run("bare run uses the default store and no file", func(t *testing.T) {
		bootFlags(t, "", "")
		b, err := resolveBoot(env)
		if err != nil {
			t.Fatal(err)
		}
		if b.filePath != "" || b.origin != originDefault || b.store.DBPath != userDB {
			t.Errorf("boot = %+v", b)
		}
	})
	t.Run("--db keeps its own store and no file", func(t *testing.T) {
		bootFlags(t, "", "/x/a.db")
		b, err := resolveBoot(env)
		if err != nil {
			t.Fatal(err)
		}
		if b.filePath != "" || b.store.DBPath != "/x/a.db" || b.origin != originFlag {
			t.Errorf("boot = %+v", b)
		}
	})
	t.Run("--config supplies the store", func(t *testing.T) {
		writeFile(t, "mine.yaml", "store:\n  db_path: ./mine.db\n")
		bootFlags(t, "mine.yaml", "")
		b, err := resolveBoot(env)
		if err != nil {
			t.Fatal(err)
		}
		if b.filePath != "mine.yaml" || b.origin != originFile || b.store.DBPath != "./mine.db" {
			t.Errorf("boot = %+v", b)
		}
	})
	t.Run("--config without a store block uses the default", func(t *testing.T) {
		writeFile(t, "nostore.yaml", "user: ada\n")
		bootFlags(t, "nostore.yaml", "")
		b, err := resolveBoot(env)
		if err != nil {
			t.Fatal(err)
		}
		if b.origin != originDefault || b.store.DBPath != userDB {
			t.Errorf("boot = %+v", b)
		}
	})
	t.Run("a missing --config is an error", func(t *testing.T) {
		bootFlags(t, "nope.yaml", "")
		if _, err := resolveBoot(env); err == nil {
			t.Fatal("want an error for a missing file")
		}
	})
}

// A bare session reads nothing from the working directory: the store is at the
// default path, no config row is saved and no feed is seeded.
func TestOpenSessionBareReadsNoFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	userDB := userStoreFile(home)
	feeds := "feeds:\n  - name: A\n    url: http://a.test/feed\n"
	writeFile(t, "configs/config.yaml", goodConfig+"profile: ./p.md\n")
	writeFile(t, "configs/feeds.yaml", feeds)
	writeFile(t, "rabbithole/config.yaml", goodConfig)
	writeFile(t, "p.md", "# Profile\n- local models\n")
	bootFlags(t, "", "")
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	s, err := openSession(cmd, sessionOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !fileExists(userDB) {
		t.Errorf("no store at %s", userDB)
	}
	if fileExists(legacyDefaultDB) {
		t.Errorf("a legacy store was created at %s", legacyDefaultDB)
	}
	if _, ok, err := s.db.ActiveConfig(t.Context()); err != nil || ok {
		t.Errorf("ActiveConfig ok = %v, err = %v; want no saved config", ok, err)
	}
	cfg, version, err := s.src.Active(t.Context())
	if err != nil || version != 0 || cfg.Inference.Provider != config.RunDefaults().Inference.Provider {
		t.Errorf("config = %+v, %d, %v; want the defaults", cfg, version, err)
	}
	doc, err := s.db.FeedsDoc(t.Context())
	if err != nil || len(doc.Feeds) != 0 {
		t.Errorf("feeds = %+v, %v; want none", doc.Feeds, err)
	}
	if _, set, err := s.db.ActiveProfileID(t.Context()); err != nil || set {
		t.Errorf("profile set = %v, err = %v; want none", set, err)
	}
}

func TestOpenSessionMissingStore(t *testing.T) {
	t.Chdir(t.TempDir())
	missing := filepath.Join(t.TempDir(), "nope.db")
	bootFlags(t, "", missing)
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	_, err := openSession(cmd, sessionOpts{mustExist: true})
	if err == nil || err.Error() != "no store at "+missing {
		t.Fatalf("mustExist err = %v", err)
	}
	s, err := openSession(cmd, sessionOpts{optional: true})
	if err != nil || s.db != nil || s.missing != missing {
		t.Fatalf("optional = %+v, %v; want a storeless session", s, err)
	}
	s.Close()
	cfg, version, err := s.src.Active(t.Context())
	if err != nil || version != 0 || cfg.Inference.Provider == "" {
		t.Fatalf("defaults = %+v, %d, %v", cfg, version, err)
	}
	if fileExists(missing) {
		t.Error("a store was created")
	}
}

// A boot with --config keeps a UI save while the file is unchanged; once the
// file changes it wins and the log names the undo command.
func TestOpenSessionReappliesConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	clearVar(t, "RABBITHOLE_INFERENCE_API_KEY")
	buf := &bytes.Buffer{}
	old := log
	log = zerolog.New(buf)
	t.Cleanup(func() { log = old })
	writeFile(t, "a.yaml", goodConfig)
	bootFlags(t, "a.yaml", "")
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	boot := func() store.ConfigVersion {
		s, err := openSession(cmd, sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		v, _, err := s.db.ActiveConfig(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := boot(); v.Version != 1 {
		t.Fatalf("first boot version = %d, want 1", v.Version)
	}
	if v := boot(); v.Version != 1 {
		t.Fatalf("second boot version = %d, want a noop on 1", v.Version)
	}
	dbPath := userStoreFile(home)
	db, err := store.OpenFrom(t.Context(), config.StoreConfig{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveConfig(t.Context(), "user: bob\n", "ui", store.ConfigSourceUI, 1); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if v := boot(); v.Version != 2 {
		t.Fatalf("boot after a UI save: version = %d, want the UI save kept (2)", v.Version)
	}
	writeFile(t, "a.yaml", goodConfig+"user: ada\n")
	buf.Reset()
	if v := boot(); v.Version != 3 {
		t.Fatalf("boot after the file changed: version = %d, want 3", v.Version)
	}
	if !strings.Contains(buf.String(), "undo with: rabbithole config rollback 2") {
		t.Errorf("no undo hint in %q", buf.String())
	}
}

// A UI save is kept across restarts even after the file was edited to match an
// earlier UI save.
func TestOpenSessionKeepsUISavesUntilTheFileChanges(t *testing.T) {
	t.Chdir(t.TempDir())
	isolatedHome(t)
	clearVar(t, "RABBITHOLE_INFERENCE_API_KEY")
	writeFile(t, "a.yaml", goodConfig)
	bootFlags(t, "a.yaml", "")
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	boot := func(save string) int {
		s, err := openSession(cmd, sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		v, _, err := s.db.ActiveConfig(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if save != "" {
			sv, err := s.db.SaveConfig(t.Context(), save, "h-"+save, store.ConfigSourceUI, v.Version)
			if err != nil {
				t.Fatal(err)
			}
			return sv.Version
		}
		return v.Version
	}
	if v := boot(""); v != 1 {
		t.Fatalf("import A: version = %d, want 1", v)
	}
	if v := boot("user: bob\n"); v != 2 {
		t.Fatalf("UI save B: version = %d, want 2", v)
	}
	writeFile(t, "a.yaml", "user: bob\n")
	if v := boot(""); v != 2 {
		t.Fatalf("file edited to B: version = %d, want a noop on 2", v)
	}
	if v := boot("user: carol\n"); v != 3 {
		t.Fatalf("UI save C: version = %d, want 3", v)
	}
	if v := boot(""); v != 3 {
		t.Fatalf("restart: version = %d, want C kept (3)", v)
	}
}

func TestLogManagedConfig(t *testing.T) {
	buf := &bytes.Buffer{}
	old := log
	log = zerolog.New(buf)
	t.Cleanup(func() { log = old })
	logManagedConfig("")
	if buf.Len() != 0 {
		t.Errorf("logged without --config: %q", buf.String())
	}
	logManagedConfig("a.yaml")
	if !strings.Contains(buf.String(),
		"config imported from a.yaml; edits in Settings are kept until the file changes") {
		t.Errorf("log = %q", buf.String())
	}
}

func TestLogLegacyConfigFile(t *testing.T) {
	buf := &bytes.Buffer{}
	old, oldCfg := log, configPath
	log = zerolog.New(buf)
	t.Cleanup(func() { log, configPath = old, oldCfg })
	t.Chdir(t.TempDir())

	configPath = ""
	logLegacyConfigFile()
	if buf.Len() != 0 {
		t.Errorf("logged with no legacy file: %q", buf.String())
	}
	if err := os.MkdirAll("configs", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyConfigFile, []byte("user: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath = "other.yaml"
	logLegacyConfigFile()
	if buf.Len() != 0 {
		t.Errorf("logged although --config was given: %q", buf.String())
	}
	configPath = ""
	logLegacyConfigFile()
	if !strings.Contains(buf.String(), "./configs/config.yaml is no longer read automatically") ||
		!strings.Contains(buf.String(), "rabbithole config import ./configs/config.yaml") {
		t.Errorf("log = %q", buf.String())
	}
}
