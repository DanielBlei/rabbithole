// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// envValueRE is the charset .env values written by Set must match. It excludes
// whitespace, quotes, newlines and NUL (injection) and `$` and `#` (which break
// the Makefile's .env include).
var envValueRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=:-]+$`)

// Source names reported by EnvFile.Source.
const (
	SourceEnvironment = "environment"
	SourceEnvFile     = "envfile"
	SourceMissing     = "missing"
)

// envWriteMu serializes writes to every .env file in the process.
var envWriteMu sync.Mutex

// EnvFile is a loaded .env file. Only RABBITHOLE_* keys are read, and only
// into variables the process environment did not already set.
type EnvFile struct {
	// Path is the absolute path of the file, whether or not it exists.
	Path string
	// Skipped counts keys ignored for not matching RABBITHOLE_*. Values are
	// never kept or logged.
	Skipped int

	supplied map[string]bool // names this file put into the process environment
}

// LoadEnvFile reads path and sets each RABBITHOLE_* variable the environment
// does not already define. A missing file is an empty EnvFile.
//
// Value rules: a line is KEY=VALUE with an optional leading `export `; blank
// lines and lines starting with # are ignored. The value is everything after the
// first `=`, trimmed of surrounding whitespace, with one matching pair of single
// or double quotes removed (the form .env.example shows for a shell). Nothing is
// expanded and `#` inside a value is kept, matching how make reads the file. An
// empty value is treated as unset.
func LoadEnvFile(path string) (*EnvFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve env file path: %w", err)
	}
	e := &EnvFile{Path: abs, supplied: map[string]bool{}}
	raw, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read env file %s: %w", abs, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := parseEnvLine(line)
		if !ok {
			continue
		}
		if !allowedNameRE.MatchString(key) {
			e.Skipped++
			continue
		}
		if value == "" {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return nil, fmt.Errorf("set %s from env file: %w", key, err)
		}
		e.supplied[key] = true
	}
	return e, nil
}

// parseEnvLine splits one line into key and unquoted value. ok is false for
// blanks, comments and lines without a key.
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(line, "export"); found && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		line = strings.TrimSpace(rest)
	}
	key, value, found := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !found || key == "" {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return key, value, true
}

// Source reports where the variable's value comes from: SourceEnvFile when this
// file supplied it, SourceEnvironment when the process had it already, and
// SourceMissing when it is unset or empty.
func (e *EnvFile) Source(name string) string {
	if e != nil && e.supplied[name] {
		return SourceEnvFile
	}
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return SourceEnvironment
	}
	return SourceMissing
}

// Set writes NAME=value into the file and the process environment. Every other
// line is preserved byte for byte. It refuses names outside the allow-list,
// values outside the safe charset, and variables the real environment sets.
func (e *EnvFile) Set(name, value string) error {
	if e == nil || e.Path == "" {
		return errors.New("no env file path to write")
	}
	if !allowedName(name) {
		return fmt.Errorf(
			"%s is not a variable rabbithole may write; names must match RABBITHOLE_*_API_KEY",
			name,
		)
	}
	if !envValueRE.MatchString(value) {
		return fmt.Errorf(
			"the value for %s has characters that cannot be stored in the env file (allowed: letters, digits and . _ ~ + / = : -)",
			name,
		)
	}

	envWriteMu.Lock()
	defer envWriteMu.Unlock()

	if v, set := os.LookupEnv(name); set && v != "" && !e.supplied[name] {
		return fmt.Errorf("%s is set in the environment; change it where it is set", name)
	}
	if err := writeEnvLine(e.Path, name, value); err != nil {
		return fmt.Errorf("cannot write %s: %w; export %s=... instead", e.Path, err, name)
	}
	if err := os.Setenv(name, value); err != nil {
		return fmt.Errorf("set %s: %w", name, err)
	}
	if e.supplied == nil {
		e.supplied = map[string]bool{}
	}
	e.supplied[name] = true
	return nil
}

// writeEnvLine replaces the last NAME= line in the file or appends one, then
// swaps the file in atomically. A new file is created with mode 0600.
func writeEnvLine(path, name, value string) error {
	// A symlinked .env is updated through the link, not replaced by a file.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	mode := os.FileMode(0o600)
	var content []byte
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		if content, err = os.ReadFile(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	lines := bytes.SplitAfter(content, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	replaced := false
	for i := len(lines) - 1; i >= 0; i-- {
		key, _, ok := parseEnvLine(string(lines[i]))
		if !ok || key != name {
			continue
		}
		line := lines[i]
		eol := ""
		if bytes.HasSuffix(line, []byte("\r\n")) {
			eol = "\r\n"
		} else if bytes.HasSuffix(line, []byte("\n")) {
			eol = "\n"
		}
		prefix := ""
		if strings.HasPrefix(strings.TrimSpace(string(line)), "export") {
			prefix = "export "
		}
		lines[i] = []byte(prefix + name + "=" + value + eol)
		replaced = true
		break
	}
	if !replaced {
		if n := len(lines); n > 0 && !bytes.HasSuffix(lines[n-1], []byte("\n")) {
			lines[n-1] = append(lines[n-1][:len(lines[n-1]):len(lines[n-1])], '\n')
		}
		lines = append(lines, []byte(name+"="+value+"\n"))
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(bytes.Join(lines, nil)); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Loaded counts the variables this file put into the process environment.
func (e *EnvFile) Loaded() int {
	if e == nil {
		return 0
	}
	return len(e.supplied)
}
