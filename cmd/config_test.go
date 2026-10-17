// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/store"
)

const feedsFile = `defaults:
  since: 7d
feeds:
  - name: Alpha
    url: https://alpha.example/feed.xml
  - name: Beta
    url: https://beta.example/feed.xml
    tags: [go]
`

// cliEnv is a scratch directory the commands run in, with the global flags reset.
type cliEnv struct {
	t   *testing.T
	dir string
	db  string
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	clearVar(t, "RABBITHOLE_INFERENCE_API_KEY")
	clearVar(t, "RABBITHOLE_SUMMARY_API_KEY")
	oldCfg, oldDB, oldEnv := configPath, dbFlag, envFile
	t.Cleanup(func() { configPath, dbFlag, envFile = oldCfg, oldDB, oldEnv })
	return &cliEnv{t: t, dir: dir, db: filepath.Join(dir, "x.db")}
}

func (e *cliEnv) path(name string) string { return filepath.Join(e.dir, name) }

func (e *cliEnv) write(name, body string) string {
	e.t.Helper()
	p := e.path(name)
	writeFile(e.t, p, body)
	return p
}

// run executes the CLI against the env's store and returns stdout and stderr.
func (e *cliEnv) run(args ...string) (string, string, error) {
	e.t.Helper()
	configPath, dbFlag, dbMaxConns, dbLifetime = "", "", 0, ""
	envFile = e.path(".env")
	configExportOut, configHistoryMax, sourcesExportOut = "", 20, ""
	e.t.Setenv(dbEnv, "")
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(append([]string{"--db", e.db}, args...))
	// cobra hands a subcommand the first context it saw, so each run sets its own.
	var setCtx func(c *cobra.Command)
	setCtx = func(c *cobra.Command) {
		c.SetContext(e.t.Context())
		for _, sub := range c.Commands() {
			setCtx(sub)
		}
	}
	setCtx(rootCmd)
	defer func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) }()
	err := rootCmd.ExecuteContext(e.t.Context())
	return stdout.String(), stderr.String(), err
}

func (e *cliEnv) mustRun(args ...string) string {
	e.t.Helper()
	out, _, err := e.run(args...)
	if err != nil {
		e.t.Fatalf("rabbithole %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func TestConfigExportOnMissingStore(t *testing.T) {
	e := newCLIEnv(t)
	out := e.mustRun("config", "export")
	if !strings.Contains(out, "no config is saved") || !strings.HasPrefix(out, "#") {
		t.Errorf("export = %q, want a comment saying nothing is saved", out)
	}
	if fileExists(e.db) {
		t.Error("export created the store")
	}
	if got := e.mustRun("config", "history"); !strings.Contains(got, "no saved config") {
		t.Errorf("history = %q", got)
	}
	if fileExists(e.db) {
		t.Error("history created the store")
	}
	// The empty export is itself a valid import file.
	file := e.write("empty.yaml", out)
	if got := e.mustRun("config", "import", file); !strings.Contains(got, "first saved version") {
		t.Errorf("import of the empty export = %q", got)
	}
}

func TestConfigExportImportRoundTrip(t *testing.T) {
	e := newCLIEnv(t)
	src := e.write("a.yaml", goodConfig)
	e.mustRun("config", "import", src)

	exported := e.path("out.yaml")
	e.mustRun("config", "export", "-o", exported)
	info, err := os.Stat(exported)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("export mode = %v, want 0644", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(exported)
	if !strings.HasPrefix(string(raw), "# rabbithole config version 1 (import:") ||
		!strings.Contains(string(raw), "docs/configuration.md") {
		t.Errorf("header = %q", strings.SplitN(string(raw), "\n", 2)[0])
	}
	out := e.mustRun("config", "import", exported)
	if !strings.Contains(out, "config unchanged, version 1 is active") {
		t.Errorf("round trip = %q, want a no-op", out)
	}
	if h := e.mustRun("config", "history"); strings.Count(h, "\n") != 2 {
		t.Errorf("history grew after a no-op:\n%s", h)
	}
}

func TestConfigImportEditedAndEarlierVersion(t *testing.T) {
	e := newCLIEnv(t)
	first := e.write("a.yaml", goodConfig)
	e.mustRun("config", "import", first)
	second := e.write("b.yaml", strings.Replace(goodConfig, "m1", "m2", 1))
	out := e.mustRun("config", "import", second)
	want := "config version 2 saved from " + second + ", replacing version 1 (import of "
	if !strings.Contains(out, want) || !strings.Contains(out, "); undo with: rabbithole config rollback 1") ||
		strings.Count(out, "undo with") != 1 {
		t.Errorf("import = %q, want %q and one undo hint", out, want)
	}
	// Content equal to an earlier version, from another file, still activates it.
	out = e.mustRun("config", "import", first)
	if !strings.Contains(out, "config version 3 saved from") || !strings.Contains(out, "replacing version 2") {
		t.Errorf("import of an earlier version's content = %q", out)
	}
	hist := e.mustRun("config", "history")
	if !strings.Contains(hist, "*3") {
		t.Errorf("history has no active marker on 3:\n%s", hist)
	}
}

func TestConfigRollback(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("config", "import", e.write("a.yaml", goodConfig))
	e.mustRun("config", "import", e.write("b.yaml", strings.Replace(goodConfig, "m1", "m2", 1)))

	out := e.mustRun("config", "rollback", "1")
	if !strings.Contains(out, "config version 3 is now active (copy of version 1); the next ingest run uses it") {
		t.Errorf("rollback = %q", out)
	}
	hist := e.mustRun("config", "history")
	rows := strings.Split(strings.TrimSpace(hist), "\n")
	if len(rows) != 4 {
		t.Fatalf("history rows = %d:\n%s", len(rows), hist)
	}
	hashOf := func(row string) string { f := strings.Fields(row); return f[len(f)-1] }
	if !strings.HasPrefix(rows[1], "*3") || !strings.Contains(rows[1], "rollback:1") {
		t.Errorf("active row = %q", rows[1])
	}
	if hashOf(rows[1]) != hashOf(rows[3]) {
		t.Errorf("rollback hash %s != target hash %s", hashOf(rows[1]), hashOf(rows[3]))
	}

	if out := e.mustRun("config", "rollback", "3"); !strings.Contains(out, "already active") {
		t.Errorf("rollback to the active version = %q", out)
	}
	if out := e.mustRun("config", "rollback", "1"); !strings.Contains(out, "already active") {
		t.Errorf("rollback to equal content = %q", out)
	}
	if h := e.mustRun("config", "history"); strings.Count(h, "\n") != 4 {
		t.Errorf("a no-op rollback saved a version:\n%s", h)
	}
	_, _, err := e.run("config", "rollback", "9")
	if err == nil || err.Error() != "no config version 9; see rabbithole config history" {
		t.Errorf("rollback to a missing version = %v", err)
	}
	if _, _, err := e.run("config", "rollback", "zero"); err == nil {
		t.Error("a non-numeric version was accepted")
	}
}

func TestConfigRollbackRefusesUnreadableRow(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("config", "import", e.write("a.yaml", goodConfig))
	e.mustRun("config", "import", e.write("b.yaml", strings.Replace(goodConfig, "m1", "m2", 1)))
	db := openTestDB(t, e.db)
	if _, err := db.SaveConfig(t.Context(), "inference: [not, a, mapping\n", "bad", "ui", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveConfig(t.Context(), "inference:\n  provider: ollama\n", "good", "ui", 3); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_, _, err := e.run("config", "rollback", "3")
	if err == nil || !strings.Contains(err.Error(), "cannot be restored") {
		t.Errorf("rollback to an unreadable row = %v", err)
	}
	if h := e.mustRun("config", "history"); !strings.HasPrefix(strings.Split(h, "\n")[1], "*4") {
		t.Errorf("active version changed:\n%s", h)
	}
}

func TestConfigFlagOnSubcommandsOnlyLocates(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("config", "import", e.write("a.yaml", goodConfig))
	other := e.write("other.yaml", strings.Replace(goodConfig, "m1", "other", 1))
	for _, args := range [][]string{
		{"config", "history"}, {"config", "export"}, {"config", "rollback", "1"}, {"sources", "export"},
	} {
		e.mustRun(append(args, "--config", other)...)
	}
	if h := e.mustRun("config", "history"); strings.Count(h, "\n") != 2 {
		t.Errorf("--config imported on a config subcommand:\n%s", h)
	}
	_, _, err := e.run("config", "import", e.path("a.yaml"), "--config", other)
	if err == nil || !strings.Contains(err.Error(), "differs from the file to import") {
		t.Errorf("import with a different --config = %v", err)
	}
	e.mustRun("config", "import", e.path("a.yaml"), "--config", e.path("a.yaml"))
}

func TestConfigNeverPrintsASecret(t *testing.T) {
	e := newCLIEnv(t)
	const literal = "sk-literal-abc123XYZ"
	src := e.write("a.yaml", goodConfig+"  api_key: "+literal+"\n")
	var all strings.Builder
	collect := func(args ...string) {
		out, errOut, err := e.run(args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		all.WriteString(out + errOut)
	}
	collect("config", "import", src)
	collect("config", "export")
	collect("config", "history")
	collect("config", "rollback", "1")
	out := e.mustRun("config", "export")
	if !strings.Contains(out, "api_key: $RABBITHOLE_INFERENCE_API_KEY") {
		t.Errorf("export lacks the reference:\n%s", out)
	}
	if strings.Contains(all.String(), literal) || strings.Contains(out, literal) {
		t.Error("the literal key reached the command output")
	}
}

func TestConfigWarnsOnUnsetKeyVariable(t *testing.T) {
	e := newCLIEnv(t)
	src := e.write("a.yaml", goodConfig+"  api_key: $RABBITHOLE_INFERENCE_API_KEY\n")
	_, errOut, err := e.run("config", "import", src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "$RABBITHOLE_INFERENCE_API_KEY, which is not set") {
		t.Errorf("stderr = %q, want the unset variable named", errOut)
	}
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "set-somewhere")
	e.mustRun(
		"config",
		"import",
		e.write("b.yaml", goodConfig+"  api_key: $RABBITHOLE_INFERENCE_API_KEY\n  max_parallel: 2\n"),
	)
	_, errOut, _ = e.run("config", "rollback", "1")
	if strings.Contains(errOut, "not set") {
		t.Errorf("warned about a variable that is set: %q", errOut)
	}
}

func TestSourcesImportExport(t *testing.T) {
	e := newCLIEnv(t)
	file := e.write("feeds.yaml", feedsFile)
	first := e.mustRun("sources", "import", file)
	if !strings.Contains(first, "added 2 feed(s), skipped 0 already known (including deleted ones)") {
		t.Errorf("first import = %q", first)
	}
	second := e.mustRun("sources", "import", file)
	if !strings.Contains(second, "added 0 feed(s), skipped 2 already known") {
		t.Errorf("second import = %q", second)
	}

	exported := e.path("out.yaml")
	e.mustRun("sources", "export", "-o", exported)
	if again := e.mustRun("sources", "import", exported); !strings.Contains(again, "added 0 feed(s)") {
		t.Errorf("export then import = %q, want 0 added", again)
	}
	out := e.mustRun("sources", "export")
	for _, want := range []string{"Alpha", "Beta", "https://alpha.example/feed.xml"} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
}

func TestSourcesImportRefusesOtherFiles(t *testing.T) {
	e := newCLIEnv(t)
	for name, body := range map[string]string{
		"config.yaml": goodConfig,
		"empty.yaml":  "# nothing\n",
		"broken.yaml": "feeds: [unclosed\n",
	} {
		if _, _, err := e.run("sources", "import", e.write(name, body)); err == nil {
			t.Errorf("%s was imported", name)
		}
	}
	if _, _, err := e.run("sources", "import", e.path("missing.yaml")); err == nil {
		t.Error("a missing file was imported")
	}
}

func TestSourcesExportOnMissingStore(t *testing.T) {
	e := newCLIEnv(t)
	out := e.mustRun("sources", "export")
	if fileExists(e.db) {
		t.Error("export created the store")
	}
	if got := e.mustRun("sources", "import", e.write("empty.yaml", out)); !strings.Contains(got, "added 0 feed(s)") {
		t.Errorf("empty export is not importable: %q", got)
	}
}

func openTestDB(t *testing.T, path string) *store.Store {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
