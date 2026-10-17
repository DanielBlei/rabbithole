// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/DanielBlei/rabbithole/internal/config"
)

// errNothingCreated is the clean exit after the user declines the new store.
// Execute maps it to success: the user chose it, and the prompt already said so.
var errNothingCreated = errors.New("no store created")

// errInterrupted is Ctrl-C at the first-boot question; Execute exits 130.
var errInterrupted = errors.New("interrupted")

// createPrompt is the terminal side of the first-boot question, replaceable in
// tests.
var createPrompt = struct {
	interactive func() bool
	in          io.Reader
	out         io.Writer
}{
	interactive: func() bool {
		return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
	},
	in:  os.Stdin,
	out: os.Stderr,
}

// prepareNewStore handles a default SQLite store that does not exist yet: it asks
// first on a terminal, then creates the private directory that will hold it. A
// store named with --db, RABBITHOLE_DB or --config is an explicit choice and
// never asks.
func prepareNewStore(ctx context.Context, b *boot) error {
	if b.origin != originDefault || b.def.Exists || b.store.IsPostgres() {
		return nil
	}
	if createPrompt.interactive() {
		ok, err := askCreateStore(ctx, b.def, b.home)
		if err != nil {
			return err
		}
		if !ok {
			return errNothingCreated
		}
		b.asked = true
	} else {
		b.hint = true
	}
	_, dataHome, err := perUserDir(userStoreEnv())
	if err != nil {
		return err
	}
	return makePrivateDir(filepath.Dir(b.store.DBPath), dataHome)
}

// makePrivateDir creates dir 0700 under parent, which is created 0755 if missing.
// A dir that already exists is left alone, mode included.
func makePrivateDir(dir, parent string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", parent, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return os.Chmod(dir, 0o700)
}

// restrictNewStore makes a store file this process just created, and the WAL
// files beside it, readable by its owner only.
func restrictNewStore(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn().Err(err).Str("path", p).Msg("could not restrict the store file to its owner")
		}
	}
}

// styler wraps s in an ANSI style, or returns it as is when colour is off.
type styler func(code, s string) string

var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleWidth is the display width of s without its ANSI codes.
func visibleWidth(s string) int { return utf8.RuneCountInString(ansiSeq.ReplaceAllString(s, "")) }

// panel is one titled block of rows in the first-boot screen.
type panel struct {
	title string
	rows  []string
}

// boxedLines draws the panels as boxes of one shared width: every line is as
// wide as the longest row of any panel. It also returns that width.
func boxedLines(panels []panel, paint styler) ([]string, int) {
	inner := 0
	for _, p := range panels {
		inner = max(inner, visibleWidth(p.title)+4)
		for _, r := range p.rows {
			inner = max(inner, visibleWidth(r)+3)
		}
	}
	dim := func(s string) string { return paint("2", s) }
	var lines []string
	for _, p := range panels {
		head := "─ " + paint("1", p.title) + " "
		fill := strings.Repeat("─", inner-visibleWidth(head))
		lines = append(lines, dim("╭─ ")+paint("1", p.title)+dim(" "+fill+"╮"))
		for _, r := range p.rows {
			pad := strings.Repeat(" ", inner-2-visibleWidth(r))
			lines = append(lines, dim("│")+"  "+r+pad+dim("│"))
		}
		lines = append(lines, dim("╰"+strings.Repeat("─", inner)+"╯"))
	}
	return lines, inner + 2
}

// plainLines is the boxless layout for a narrow or dumb terminal.
func plainLines(panels []panel, paint styler) []string {
	var lines []string
	for i, p := range panels {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, paint("1", p.title+":"))
		for _, r := range p.rows {
			lines = append(lines, "  "+r)
		}
	}
	return lines
}

// storeIntro renders the first-boot screen up to the question. width is the
// terminal width, 0 when unknown.
func storeIntro(def defaultStore, home string, paint styler, width int, dumb bool) string {
	shown := tildePath(def.PerUser, home)
	legacy := func(p string) string { return fmt.Sprintf("%-31s  %s", p, paint("2", "legacy")) }
	looked := panel{"Looked in", []string{shown, legacy(legacyDefaultDB), legacy(legacyInstallerDB)}}
	const label = 14
	pad := strings.Repeat(" ", label)
	other := panel{"Other choices", []string{
		fmt.Sprintf("%-*s", label, "Postgres") + "answer n, then set",
		pad + "  " + paint("36", dbEnv) + "=postgres://user@host:5432/rabbithole",
		pad + "  " + paint("36", config.DBPasswordEnv) + " in .env or your shell profile",
		fmt.Sprintf("%-*s", label, "Another file") + paint("36", "--db") + " <path>, or " +
			paint("36", dbEnv) + "=<path>",
	}}
	panels := []panel{looked, other}
	lines, w := boxedLines(panels, paint)
	if dumb || (width > 0 && width < w) {
		lines = plainLines(panels, paint)
	}
	rabbit := []string{`(\(\`, `( o.o)`, `o_(")(")`}
	text := []string{paint("1", "The Rabbit Hole"), paint("2", "No store yet. Let's set one up."), ""}
	var sb strings.Builder
	sb.WriteString("\n")
	for i, r := range rabbit {
		sb.WriteString(strings.TrimRight("   "+paint("2", fmt.Sprintf("%-11s", r))+text[i], " ") + "\n")
	}
	sb.WriteString("\n" + strings.Join(lines, "\n") + "\n\n")
	return sb.String()
}

// terminalWidth is the width of the error stream's terminal, 0 when unknown.
var terminalWidth = func() int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil {
		return 0
	}
	return w
}

// newPainter colours output only on a terminal with NO_COLOR unset.
func newPainter() styler {
	color := createPrompt.interactive() && os.Getenv("NO_COLOR") == ""
	return func(code, s string) string {
		if !color {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
}

// announceNewStore confirms a store the user agreed to create, once it exists.
func announceNewStore(shown string) {
	_, _ = io.WriteString(createPrompt.out, newPainter()("32", "✔ Store created at "+shown)+"\n\n")
}

// askCreateStore prints what was searched and how to choose differently, then
// asks whether to create the default store. Ctrl-C ends it at once.
func askCreateStore(ctx context.Context, def defaultStore, home string) (bool, error) {
	out := createPrompt.out
	paint := newPainter()
	shown := tildePath(def.PerUser, home)
	_, _ = io.WriteString(out, storeIntro(def, home, paint, terminalWidth(), os.Getenv("TERM") == "dumb"))
	_, _ = io.WriteString(out, paint("1;36", "?")+" Create a new SQLite store at "+paint("1", shown)+"? [Y/n]: ")

	in := bufio.NewReader(createPrompt.in)
	for {
		line, eof, err := readAnswer(ctx, in)
		if err != nil {
			if ctx.Err() != nil {
				_, _ = io.WriteString(out, "\n")
				return false, errInterrupted
			}
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			if line == "" && eof {
				return declineStore(out, paint, true), nil
			}
			return true, nil
		case "n", "no":
			return declineStore(out, paint, eof), nil
		}
		if eof {
			return declineStore(out, paint, true), nil
		}
		_, _ = io.WriteString(out, "  Please answer y or n: ")
	}
}

// declineStore says nothing was created and how to choose a store. afterEOF adds
// the newline a terminal did not echo.
func declineStore(out io.Writer, paint styler, afterEOF bool) bool {
	if afterEOF {
		_, _ = io.WriteString(out, "\n")
	}
	_, _ = io.WriteString(out, paint("31", "✘ Nothing created.")+"\nSet "+paint("36", dbEnv)+" (plus "+
		paint("36", config.DBPasswordEnv)+" for Postgres) or pass "+paint("36", "--db")+",\nthen run "+
		invocation()+" again.\n")
	return false
}

// invoked is the path of the command being run, e.g. "rabbithole items list".
var invoked string

// invocation is the command the user ran, for "run it again" hints.
func invocation() string {
	if invoked != "" {
		return invoked
	}
	return "rabbithole"
}

// readAnswer reads one line in a goroutine so a cancelled context ends the wait
// without a keypress. The goroutine ends with the process.
func readAnswer(ctx context.Context, r *bufio.Reader) (line string, eof bool, err error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, err := r.ReadString('\n')
		ch <- result{l, err}
	}()
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case res := <-ch:
		if res.err != nil && !errors.Is(res.err, io.EOF) {
			return "", false, fmt.Errorf("reading the answer: %w", res.err)
		}
		return strings.TrimRight(res.line, "\r\n"), res.err != nil, nil
	}
}

var storeGroupCmd = &cobra.Command{
	Use:   "store",
	Short: "Inspect where the store lives",
}

func init() {
	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print where the store is, and how that was decided",
		Long: "Prints the store this command line would use and where the choice came from, without " +
			"creating anything. A Postgres store prints its host and database, never the password.",
		Args: cobra.NoArgs,
		RunE: runStorePath,
	}
	storeGroupCmd.AddCommand(pathCmd)
	rootCmd.AddCommand(storeGroupCmd)
}

func runStorePath(cmd *cobra.Command, _ []string) error {
	if _, err := loadEnv(); err != nil {
		return err
	}
	b, err := resolveBoot(userStoreEnv())
	if err != nil {
		return err
	}
	line, err := describeStore(b)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
	return err
}

// describeStore is the `store path` line for a resolved boot.
func describeStore(b boot) (string, error) {
	if b.store.IsPostgres() {
		pg, err := b.store.ResolvePostgres()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s  (postgres, %s)", pg.Label, originName(b.origin)), nil
	}
	shown := tildePath(b.store.DBPath, b.home)
	switch b.origin {
	case originDefault, originLegacy:
		state := "not created yet"
		if b.def.Exists {
			state = "exists"
		}
		return fmt.Sprintf("%s  (sqlite, %s, %s)", shown, b.origin, state), nil
	}
	return fmt.Sprintf("%s  (sqlite, %s)", shown, originName(b.origin)), nil
}

// originName is how a location's source reads to people.
func originName(o storeOrigin) string {
	switch o {
	case originFlag:
		return "--db"
	case originEnv:
		return dbEnv
	case originFile:
		return "--config"
	}
	return string(o)
}
