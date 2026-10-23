// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/store"
)

// TestMain keeps every test off the developer's own home.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rabbithole-cmd-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", dir)
	_ = os.Unsetenv("XDG_DATA_HOME")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestLocateDefaultStore(t *testing.T) {
	const (
		linuxDB  = "/home/ada/.local/share/rabbithole/rabbithole.db"
		xdgDB    = "/data/xdg/rabbithole/rabbithole.db"
		darwinDB = "/Users/ada/Library/Application Support/rabbithole/rabbithole.db"
	)
	tests := []struct {
		name        string
		env         storeEnv
		wantPath    string
		wantLegacy  bool
		wantExists  bool
		wantIgnored []string
		wantErr     string
	}{
		{name: "linux nothing exists", env: fakeEnv("/home/ada", nil), wantPath: linuxDB},
		{
			name:     "xdg absolute",
			env:      fakeEnv("/home/ada", map[string]string{"XDG_DATA_HOME": "/data/xdg"}),
			wantPath: xdgDB,
		},
		{
			name:     "xdg relative is ignored",
			env:      fakeEnv("/home/ada", map[string]string{"XDG_DATA_HOME": "rel/share"}),
			wantPath: linuxDB,
		},
		{
			name:     "xdg absolute needs no home",
			env:      fakeEnv("", map[string]string{"XDG_DATA_HOME": "/data/xdg"}),
			wantPath: xdgDB,
		},
		{
			name: "darwin",
			env: storeEnv{
				getenv: func(string) string { return "" }, home: "/Users/ada", goos: "darwin",
				exists: existsOnly(),
			},
			wantPath: darwinDB,
		},
		{
			name: "darwin ignores xdg",
			env: storeEnv{
				getenv: func(string) string { return "/data/xdg" }, home: "/Users/ada", goos: "darwin",
				exists: existsOnly(),
			},
			wantPath: darwinDB,
		},
		{name: "no home", env: fakeEnv("", nil), wantErr: "cannot find a home directory for the default store"},
		{
			name: "no home on darwin",
			env: storeEnv{
				getenv: func(string) string { return "" }, goos: "darwin", exists: existsOnly(),
			},
			wantErr: "cannot find a home directory",
		},
		{
			name: "per-user exists", env: fakeEnv("/home/ada", nil, linuxDB),
			wantPath: linuxDB, wantExists: true,
		},
		{
			name: "legacy default only", env: fakeEnv("/home/ada", nil, legacyDefaultDB),
			wantPath: legacyDefaultDB, wantLegacy: true, wantExists: true,
		},
		{
			name: "legacy installer only", env: fakeEnv("/home/ada", nil, legacyInstallerDB),
			wantPath: legacyInstallerDB, wantLegacy: true, wantExists: true,
		},
		{
			name:        "both legacy: the default one wins",
			env:         fakeEnv("/home/ada", nil, legacyDefaultDB, legacyInstallerDB),
			wantPath:    legacyDefaultDB,
			wantLegacy:  true,
			wantExists:  true,
			wantIgnored: []string{legacyInstallerDB},
		},
		{
			name:        "per-user beats both legacy",
			env:         fakeEnv("/home/ada", nil, linuxDB, legacyDefaultDB, legacyInstallerDB),
			wantPath:    linuxDB,
			wantExists:  true,
			wantIgnored: []string{legacyDefaultDB, legacyInstallerDB},
		},
		{
			name: "per-user beats one legacy", env: fakeEnv("/home/ada", nil, linuxDB, legacyInstallerDB),
			wantPath: linuxDB, wantExists: true, wantIgnored: []string{legacyInstallerDB},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := locateDefaultStore(tc.env)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Path != tc.wantPath || got.Legacy != tc.wantLegacy || got.Exists != tc.wantExists {
				t.Errorf("got %+v", got)
			}
			if strings.Join(got.Ignored, ",") != strings.Join(tc.wantIgnored, ",") {
				t.Errorf("ignored = %v, want %v", got.Ignored, tc.wantIgnored)
			}
		})
	}
}

func TestTildePath(t *testing.T) {
	tests := []struct{ path, home, want string }{
		{"/home/ada/.local/share/x.db", "/home/ada", "~/.local/share/x.db"},
		{"/home/ada", "/home/ada", "~"},
		{"/home/adam/x.db", "/home/ada", "/home/adam/x.db"},
		{"./data/x.db", "/home/ada", "./data/x.db"},
		{"/srv/x.db", "", "/srv/x.db"},
	}
	for _, tc := range tests {
		if got := tildePath(tc.path, tc.home); got != tc.want {
			t.Errorf("tildePath(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
		}
	}
}

// capturePrompt swaps the terminal side of the question for the test.
func capturePrompt(t *testing.T, interactive bool, input string) *bytes.Buffer {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	old := createPrompt
	out := &bytes.Buffer{}
	createPrompt.interactive = func() bool { return interactive }
	createPrompt.in = strings.NewReader(input)
	createPrompt.out = out
	t.Cleanup(func() { createPrompt = old })
	return out
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := log
	log = zerolog.New(buf)
	t.Cleanup(func() { log = old })
	return buf
}

func sessionCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := &cobra.Command{}
	c.SetContext(t.Context())
	return c
}

// osDataHome is the per-user data home, relative to HOME, on the OS under test.
func osDataHome() string {
	if runtime.GOOS == "darwin" {
		return "Library/Application Support"
	}
	return ".local/share"
}

func userStoreFile(home string) string {
	return filepath.Join(home, filepath.FromSlash(osDataHome()), "rabbithole", "rabbithole.db")
}

// userStoreTilde is userStoreFile as the CLI prints it, with HOME shortened.
func userStoreTilde() string { return "~/" + osDataHome() + "/rabbithole/rabbithole.db" }

// dataHomeRoot is the first directory a store creation would add under home.
func dataHomeRoot(home string) string {
	return filepath.Join(home, strings.SplitN(osDataHome(), "/", 2)[0])
}

func TestAskCreateStore(t *testing.T) {
	def := defaultStore{PerUser: "/home/ada/.local/share/rabbithole/rabbithole.db"}
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"enter", "\n", true},
		{"y", "y\n", true},
		{"yes any case", "YES\n", true},
		{"y without newline", "y", true},
		{"n", "n\n", false},
		{"no", "No\n", false},
		{"eof declines", "", false},
		{"retries after junk", "maybe\ny\n", true},
		{"junk then eof declines", "maybe", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := capturePrompt(t, true, tc.input)
			got, err := askCreateStore(t.Context(), def, "/home/ada")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
			text := out.String()
			if !strings.Contains(
				text,
				"Create a new SQLite store at ~/.local/share/rabbithole/rabbithole.db? [Y/n]: ",
			) {
				t.Errorf("no question in %q", text)
			}
			if strings.Contains(text, "\x1b") {
				t.Errorf("colour with NO_COLOR set: %q", text)
			}
			if declined := strings.Contains(text, "Nothing created.\nSet RABBITHOLE_DB"); declined == tc.want {
				t.Errorf("decline line present = %v for answer %v", declined, tc.want)
			}
		})
	}
}

func fixedWidth(t *testing.T, w int) {
	t.Helper()
	old := terminalWidth
	terminalWidth = func() int { return w }
	t.Cleanup(func() { terminalWidth = old })
}

func askDefault(t *testing.T) {
	t.Helper()
	def := defaultStore{PerUser: "/home/ada/.local/share/rabbithole/rabbithole.db"}
	if _, err := askCreateStore(t.Context(), def, "/home/ada"); err != nil {
		t.Fatal(err)
	}
}

func TestAskCreateStoreBoxed(t *testing.T) {
	out := capturePrompt(t, true, "\n")
	fixedWidth(t, 120)
	askDefault(t)
	text := out.String()
	var widths []int
	for _, l := range strings.Split(text, "\n") {
		if strings.ContainsAny(l, "╭│╰") {
			widths = append(widths, visibleWidth(l))
		}
	}
	if len(widths) != 11 {
		t.Fatalf("boxed lines = %d in\n%s", len(widths), text)
	}
	for _, w := range widths {
		if w != widths[0] {
			t.Fatalf("box widths differ: %v in\n%s", widths, text)
		}
	}
	for _, frag := range []string{
		"   (\\(\\       The Rabbit Hole\n   ( o.o)     No store yet. Let's set one up.\n   o_(\")(\")\n",
		"│  ./data/rabbithole.db             legacy",
		"│  Postgres      answer n, then set",
		"RABBITHOLE_DB=postgres://user@host:5432/rabbithole",
		"│  Another file  --db <path>, or RABBITHOLE_DB=<path>",
		"? Create a new SQLite store at ~/.local/share/rabbithole/rabbithole.db? [Y/n]: ",
	} {
		if !strings.Contains(text, frag) {
			t.Errorf("missing %q in\n%s", frag, text)
		}
	}
}

func TestAskCreateStoreNarrowFallback(t *testing.T) {
	out := capturePrompt(t, true, "n\n")
	fixedWidth(t, 40)
	askDefault(t)
	text := out.String()
	if strings.ContainsAny(text, "╭│╰") {
		t.Errorf("boxes on a narrow terminal:\n%s", text)
	}
	for _, frag := range []string{"Looked in:\n  ~/.local/share/rabbithole/rabbithole.db\n",
		"Other choices:\n  Postgres", "(\\(\\", "✘ Nothing created.\nSet RABBITHOLE_DB"} {
		if !strings.Contains(text, frag) {
			t.Errorf("missing %q in\n%s", frag, text)
		}
	}
	t.Setenv("TERM", "dumb")
	out.Reset()
	createPrompt.in = strings.NewReader("n\n")
	fixedWidth(t, 200)
	askDefault(t)
	if strings.ContainsAny(out.String(), "╭│╰") {
		t.Error("boxes with TERM=dumb")
	}
}

func TestAskCreateStoreColour(t *testing.T) {
	out := capturePrompt(t, true, "n\n")
	t.Setenv("NO_COLOR", "")
	def := defaultStore{PerUser: "/home/ada/.local/share/rabbithole/rabbithole.db"}
	if _, err := askCreateStore(t.Context(), def, "/home/ada"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[") {
		t.Error("no colour on a terminal without NO_COLOR")
	}
}

// blockingReader never returns, like a terminal with nothing typed.
type blockingReader struct{ done chan struct{} }

func (r blockingReader) Read([]byte) (int, error) { <-r.done; return 0, context.Canceled }

func TestAskCreateStoreCancel(t *testing.T) {
	out := capturePrompt(t, true, "")
	r := blockingReader{done: make(chan struct{})}
	defer close(r.done)
	createPrompt.in = r
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := askCreateStore(ctx, defaultStore{PerUser: "/x/rabbithole.db"}, "")
	if err != errInterrupted {
		t.Fatalf("err = %v, want errInterrupted", err)
	}
	if strings.Contains(out.String(), "Nothing created") {
		t.Error("a cancel is not a decline")
	}
}

func TestOpenSessionCreatesDefaultStore(t *testing.T) {
	t.Run("terminal yes creates it private", func(t *testing.T) {
		t.Chdir(t.TempDir())
		home := isolatedHome(t)
		bootFlags(t, "", "")
		out := capturePrompt(t, true, "\n")
		buf := captureLog(t)
		s, err := openSession(sessionCmd(t), sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		db := userStoreFile(home)
		assertMode(t, filepath.Dir(db), 0o700)
		assertMode(t, db, 0o600)
		if !strings.Contains(buf.String(), `"level":"debug"`) || !strings.Contains(buf.String(), "store created") ||
			strings.Contains(buf.String(), "hint") {
			t.Errorf("log = %s", buf.String())
		}
		if !strings.Contains(out.String(), "✔ Store created at "+userStoreTilde()+"\n\n") {
			t.Errorf("no confirmation in %q", out.String())
		}
	})
	t.Run("terminal no creates nothing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		home := isolatedHome(t)
		bootFlags(t, "", "")
		capturePrompt(t, true, "n\n")
		_, err := openSession(sessionCmd(t), sessionOpts{})
		if err != errNothingCreated {
			t.Fatalf("err = %v, want errNothingCreated", err)
		}
		if _, err := os.Stat(dataHomeRoot(home)); err == nil {
			t.Error("a directory was created")
		}
	})
	t.Run("no terminal creates and says so", func(t *testing.T) {
		t.Chdir(t.TempDir())
		home := isolatedHome(t)
		bootFlags(t, "", "")
		out := capturePrompt(t, false, "")
		buf := captureLog(t)
		s, err := openSession(sessionCmd(t), sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		assertMode(t, userStoreFile(home), 0o600)
		if out.Len() != 0 {
			t.Errorf("prompt written without a terminal: %q", out.String())
		}
		want := `"engine":"sqlite","store":"` + userStoreTilde() + `","hint":"set RABBITHOLE_DB for Postgres or another path","message":"store created"`
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log = %s", buf.String())
		}
	})
	t.Run("later boot connects", func(t *testing.T) {
		t.Chdir(t.TempDir())
		isolatedHome(t)
		bootFlags(t, "", "")
		capturePrompt(t, false, "")
		for range 2 {
			s, err := openSession(sessionCmd(t), sessionOpts{})
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		}
		buf := captureLog(t)
		s, err := openSession(sessionCmd(t), sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		if !strings.Contains(buf.String(), `"from":"default","message":"store connected"`) ||
			strings.Contains(buf.String(), "created") {
			t.Errorf("log = %s", buf.String())
		}
	})
	t.Run("an existing private dir keeps its mode", func(t *testing.T) {
		t.Chdir(t.TempDir())
		home := isolatedHome(t)
		dir := filepath.Dir(userStoreFile(home))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		bootFlags(t, "", "")
		capturePrompt(t, false, "")
		s, err := openSession(sessionCmd(t), sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		assertMode(t, dir, 0o750)
	})
	t.Run("a named store is created without asking and untouched", func(t *testing.T) {
		t.Chdir(t.TempDir())
		isolatedHome(t)
		named := filepath.Join(t.TempDir(), "sub", "mine.db")
		bootFlags(t, "", named)
		out := capturePrompt(t, true, "n\n")
		s, err := openSession(sessionCmd(t), sessionOpts{})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		if out.Len() != 0 {
			t.Errorf("asked about a named store: %q", out.String())
		}
		assertMode(t, filepath.Dir(named), 0o755&^currentUmask())
		assertMode(t, named, 0o644&^currentUmask())
	})
}

func TestOpenSessionLegacyStore(t *testing.T) {
	t.Chdir(t.TempDir())
	isolatedHome(t)
	writeLegacyStore(t, legacyDefaultDB)
	writeFile(t, legacyInstallerDB, "x")
	bootFlags(t, "", "")
	out := capturePrompt(t, true, "n\n")
	buf := captureLog(t)
	s, err := openSession(sessionCmd(t), sessionOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if out.Len() != 0 {
		t.Errorf("asked with a legacy store present: %q", out.String())
	}
	logged := buf.String()
	for _, want := range []string{
		`using legacy store ./data/rabbithole.db (deprecated, supported until v0.4.0); move it to ~/` + osDataHome() + `/rabbithole/ or pass --db ./data/rabbithole.db to keep it here`,
		`legacy store ./rabbithole/data/rabbithole.db is ignored; using ./data/rabbithole.db`,
		`"from":"legacy"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	if fileExists(userStoreFile(os.Getenv("HOME"))) {
		t.Error("a per-user store was created beside the legacy one")
	}
}

func TestOpenSessionPerUserStoreBeatsLegacy(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	writeLegacyStore(t, legacyDefaultDB)
	bootFlags(t, "", "")
	capturePrompt(t, false, "")
	s, err := openSession(sessionCmd(t), sessionOpts{})
	if err == nil {
		s.Close()
	}
	// The legacy store is in use first; once a per-user store exists it wins.
	writeLegacyStore(t, userStoreFile(home))
	buf := captureLog(t)
	s, err = openSession(sessionCmd(t), sessionOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if !strings.Contains(
		buf.String(),
		"legacy store ./data/rabbithole.db is ignored; using "+userStoreTilde(),
	) ||
		strings.Contains(buf.String(), "deprecated") {
		t.Errorf("log = %s", buf.String())
	}
}

func TestReadOnlySessionNeverCreates(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	bootFlags(t, "", "")
	out := capturePrompt(t, true, "y\n")
	s, err := openSession(sessionCmd(t), sessionOpts{optional: true, noImport: true})
	if err != nil || s.db != nil {
		t.Fatalf("optional = %+v, %v", s, err)
	}
	s.Close()
	if s.missing != userStoreTilde() {
		t.Errorf("missing = %q", s.missing)
	}
	_, err = openSession(sessionCmd(t), sessionOpts{mustExist: true})
	if err == nil || err.Error() != "no store at "+userStoreTilde() {
		t.Errorf("mustExist err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("read-only command asked: %q", out.String())
	}
	if _, err := os.Stat(dataHomeRoot(home)); err == nil {
		t.Error("a read-only command created a directory")
	}
}

func TestDescribeStore(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	userDB := userStoreFile(home)

	describe := func(db string) string {
		t.Helper()
		bootFlags(t, "", db)
		b, err := resolveBoot(userStoreEnv())
		if err != nil {
			t.Fatal(err)
		}
		line, err := describeStore(b)
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	if got, want := describe(
		"",
	), userStoreTilde()+"  (sqlite, default, not created yet)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if fileExists(userDB) {
		t.Fatal("store path created the store")
	}
	writeFile(t, userDB, "x")
	if got, want := describe(""), userStoreTilde()+"  (sqlite, default, exists)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if err := os.Remove(userDB); err != nil {
		t.Fatal(err)
	}
	writeFile(t, legacyDefaultDB, "x")
	if got, want := describe(""), "./data/rabbithole.db  (sqlite, legacy, exists)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := describe("/srv/a.db"), "/srv/a.db  (sqlite, --db)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got := describe("postgres://ada:hunter2@db.example:5432/rh")
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "(postgres, --db)") {
		t.Errorf("postgres line = %q", got)
	}
	t.Setenv(dbEnv, "postgres://ada@db.example:5432/rh")
	bootFlags(t, "", "")
	t.Setenv(dbEnv, "postgres://ada@db.example:5432/rh")
	b, err := resolveBoot(userStoreEnv())
	if err != nil {
		t.Fatal(err)
	}
	line, err := describeStore(b)
	if err != nil || !strings.HasSuffix(line, "(postgres, RABBITHOLE_DB)") {
		t.Errorf("env postgres line = %q, %v", line, err)
	}
}

func TestStorePathCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	bootFlags(t, "", "")
	out := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(out)
	c.SetContext(t.Context())
	if err := runStorePath(c, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), userStoreTilde()+"  (sqlite, default, not created yet)\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := os.Stat(dataHomeRoot(home)); err == nil {
		t.Error("store path created a directory")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func currentUmask() os.FileMode {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return os.FileMode(old)
}

// writeLegacyStore creates a real, empty SQLite store at path.
func writeLegacyStore(t *testing.T, path string) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

func TestItemsReadCommandsDoNotCreate(t *testing.T) {
	t.Chdir(t.TempDir())
	home := isolatedHome(t)
	bootFlags(t, "", "")
	out := capturePrompt(t, true, "y\n")
	for _, run := range []func(*cobra.Command, []string) error{runList, runSources} {
		if err := run(sessionCmd(t), nil); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() != 0 {
		t.Errorf("asked: %q", out.String())
	}
	if _, err := os.Stat(dataHomeRoot(home)); err == nil {
		t.Error("a read command created a directory")
	}
}

func TestDeclineNamesTheCommand(t *testing.T) {
	old := invoked
	invoked = "rabbithole items rate"
	t.Cleanup(func() { invoked = old })
	out := &bytes.Buffer{}
	declineStore(out, func(_, s string) string { return s }, false)
	if !strings.Contains(out.String(), "run rabbithole items rate again.") {
		t.Errorf("out = %q", out.String())
	}
}
