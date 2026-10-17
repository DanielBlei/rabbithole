// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFile(t *testing.T) {
	path := writeEnv(t, `# comment
RABBITHOLE_A=one
export RABBITHOLE_B="two words"
RABBITHOLE_C='three'
RABBITHOLE_D=pa#ss$word

RABBITHOLE_EMPTY=
AWS_SECRET=nope
lowercase=nope
RABBITHOLE_ENV=fromfile
not a line
`)
	t.Setenv("RABBITHOLE_ENV", "fromenv")
	for _, n := range []string{"RABBITHOLE_A", "RABBITHOLE_B", "RABBITHOLE_C", "RABBITHOLE_D", "RABBITHOLE_EMPTY", "AWS_SECRET"} {
		t.Setenv(n, "")
		_ = os.Unsetenv(n)
	}
	e, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(e.Path) {
		t.Errorf("path = %q", e.Path)
	}
	for name, want := range map[string]string{
		"RABBITHOLE_A": "one", "RABBITHOLE_B": "two words", "RABBITHOLE_C": "three",
		"RABBITHOLE_D": "pa#ss$word", "RABBITHOLE_ENV": "fromenv",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, ok := os.LookupEnv("AWS_SECRET"); ok {
		t.Error("non RABBITHOLE key was loaded")
	}
	if _, ok := os.LookupEnv("RABBITHOLE_EMPTY"); ok {
		t.Error("empty value was set")
	}
	if e.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", e.Skipped)
	}
	wantSrc := map[string]string{
		"RABBITHOLE_A": SourceEnvFile, "RABBITHOLE_ENV": SourceEnvironment, "RABBITHOLE_NOPE": SourceMissing,
	}
	for name, want := range wantSrc {
		if got := e.Source(name); got != want {
			t.Errorf("Source(%s) = %q, want %q", name, got, want)
		}
	}
	var nilFile *EnvFile
	if nilFile.Source("RABBITHOLE_NOPE") != SourceMissing {
		t.Error("nil EnvFile must report missing")
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	e, err := LoadEnvFile(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil || e == nil || e.Skipped != 0 {
		t.Fatalf("missing file = %+v, %v", e, err)
	}
}

func TestEnvFileSet(t *testing.T) {
	const name = "RABBITHOLE_INFERENCE_API_KEY"
	t.Run("creates 0600 and sets the environment", func(t *testing.T) {
		_ = os.Unsetenv(name)
		t.Cleanup(func() { _ = os.Unsetenv(name) })
		path := filepath.Join(t.TempDir(), ".env")
		e, _ := LoadEnvFile(path)
		if err := e.Set(name, "sk-abc.DEF1"); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", info.Mode().Perm())
		}
		if got, _ := os.ReadFile(path); string(got) != name+"=sk-abc.DEF1\n" {
			t.Errorf("file = %q", got)
		}
		if os.Getenv(name) != "sk-abc.DEF1" || e.Source(name) != SourceEnvFile {
			t.Errorf("env = %q source = %q", os.Getenv(name), e.Source(name))
		}
	})
	t.Run("replaces in place and preserves other lines", func(t *testing.T) {
		_ = os.Unsetenv(name)
		t.Cleanup(func() { _ = os.Unsetenv(name) })
		body := "# top comment\nFOREIGN='x y' # keep\n\n" + name + "=old\nRABBITHOLE_DB_PASSWORD=pw$#\n# tail"
		path := writeEnv(t, body)
		e, _ := LoadEnvFile(path)
		_ = os.Unsetenv(name) // loaded from the file above; a new Set must still replace
		e.supplied[name] = true
		if err := e.Set(name, "newvalue"); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(body, name+"=old", name+"=newvalue", 1)
		got, _ := os.ReadFile(path)
		if string(got) != want {
			t.Errorf("file =\n%q\nwant\n%q", got, want)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o640 {
			t.Errorf("existing mode changed to %v", info.Mode().Perm())
		}
		_ = os.Unsetenv("RABBITHOLE_DB_PASSWORD")
	})
	t.Run("appends when absent, fixing a missing final newline", func(t *testing.T) {
		_ = os.Unsetenv(name)
		t.Cleanup(func() { _ = os.Unsetenv(name) })
		path := writeEnv(t, "FOREIGN=1")
		e, _ := LoadEnvFile(path)
		if err := e.Set(name, "v1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != "FOREIGN=1\n"+name+"=v1\n" {
			t.Errorf("file = %q", got)
		}
	})
	t.Run("refuses bad input", func(t *testing.T) {
		path := writeEnv(t, "")
		e, _ := LoadEnvFile(path)
		for _, v := range []string{"", "a b", "a\nB=c", "a\x00b", "a$b", "a#b", `a"b`, "a'b"} {
			if err := e.Set(name, v); err == nil {
				t.Errorf("value %q accepted", v)
			}
		}
		for _, n := range []string{"AWS_SECRET", "RABBITHOLE_DB_PASSWORD", "RABBITHOLE_DB", "RABBITHOLE_X", "rabbithole_x", "RABBITHOLE_"} {
			if err := e.Set(n, "v"); err == nil {
				t.Errorf("name %q accepted", n)
			}
		}
		if got, _ := os.ReadFile(path); len(got) != 0 {
			t.Errorf("file written despite refusals: %q", got)
		}
	})
	t.Run("refuses a variable the environment sets", func(t *testing.T) {
		t.Setenv(name, "external")
		path := filepath.Join(t.TempDir(), ".env")
		e, _ := LoadEnvFile(path)
		err := e.Set(name, "v")
		if err == nil || !strings.Contains(err.Error(), "set in the environment; change it where it is set") {
			t.Errorf("err = %v", err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Error("file created")
		}
		if os.Getenv(name) != "external" {
			t.Error("environment overwritten")
		}
	})
	t.Run("unwritable location suggests export", func(t *testing.T) {
		_ = os.Unsetenv(name)
		e := &EnvFile{Path: filepath.Join(t.TempDir(), "missing-dir", ".env"), supplied: map[string]bool{}}
		err := e.Set(name, "secretvalue")
		if err == nil || !strings.Contains(err.Error(), "export "+name+"=") || !strings.Contains(err.Error(), e.Path) {
			t.Errorf("err = %v", err)
		}
		if strings.Contains(err.Error(), "secretvalue") {
			t.Error("error echoes the value")
		}
	})
}

func TestEnvFileSetConcurrent(t *testing.T) {
	path := writeEnv(t, "# keep\nFOREIGN=1\n")
	e, _ := LoadEnvFile(path)
	const n = 16
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("RABBITHOLE_CONC_%d_API_KEY", i)
		_ = os.Unsetenv(names[i])
	}
	t.Cleanup(func() {
		for _, nm := range names {
			_ = os.Unsetenv(nm)
		}
	})
	var wg sync.WaitGroup
	for i := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Set(names[i], fmt.Sprintf("v%d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), "# keep\nFOREIGN=1\n") {
		t.Errorf("foreign lines damaged: %q", got)
	}
	for i, nm := range names {
		if !strings.Contains(string(got), fmt.Sprintf("%s=v%d\n", nm, i)) {
			t.Errorf("%s missing from %q", nm, got)
		}
	}
}

func TestEnvFileSetThroughSymlink(t *testing.T) {
	const name = "RABBITHOLE_LINKED_API_KEY"
	_ = os.Unsetenv(name)
	t.Cleanup(func() { _ = os.Unsetenv(name) })
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("FOREIGN=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	e, _ := LoadEnvFile(link)
	if err := e.Set(name, "v1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "FOREIGN=1\n"+name+"=v1\n" {
		t.Errorf("target = %q", got)
	}
}

func TestEnvFileLoadedCount(t *testing.T) {
	const name = "RABBITHOLE_TEST_LOADED_COUNT"
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(name) })
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(name+"=abc\nOTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if e.Loaded() != 1 {
		t.Errorf("Loaded = %d, want 1", e.Loaded())
	}
	var none *EnvFile
	if none.Loaded() != 0 {
		t.Error("a nil env file must count zero")
	}
}

func TestEnvFileSetOverwritesEmptyVariable(t *testing.T) {
	const name = "RABBITHOLE_EMPTYSET_API_KEY"
	t.Setenv(name, "")
	path := filepath.Join(t.TempDir(), ".env")
	e, _ := LoadEnvFile(path)
	if err := e.Set(name, "v1"); err != nil {
		t.Fatalf("Set on an empty variable: %v", err)
	}
	if os.Getenv(name) != "v1" {
		t.Error("value not applied")
	}
}
