// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	// rootCmd is reused across invocations in tests. Cobra keeps a flag's
	// Changed bit, so clear it to model a fresh process for each run.
	if f := rootCmd.PersistentFlags().Lookup("config"); f != nil {
		f.Changed = false
	}
	envFile = e.path(".env")
	configExportOut, configExportFormat, configExportDefaults = "", "", false
	configHistoryMax, sourcesExportOut = 20, ""
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

func TestConfigFlagDeprecationNotice(t *testing.T) {
	const notice = "Warning: --config is deprecated. Use 'rabbithole config import FILE' instead."

	e := newCLIEnv(t)
	cfg := e.write("a.yaml", goodConfig)

	t.Run("explicit flag prints once to stderr", func(t *testing.T) {
		stdout, stderr, err := e.run("config", "history", "--config", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(stderr, notice); got != 1 {
			t.Fatalf("stderr has %d notices, want 1:\n%s", got, stderr)
		}
		if strings.Contains(stdout, notice) {
			t.Errorf("the notice reached stdout: %q", stdout)
		}
	})

	t.Run("no flag prints no notice", func(t *testing.T) {
		_, stderr, err := e.run("config", "history")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, notice) {
			t.Errorf("stderr = %q, want no deprecation notice", stderr)
		}
	})

	t.Run("config import still works without a notice", func(t *testing.T) {
		stdout, stderr, err := e.run("config", "import", e.write("import.yaml", goodConfig))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, notice) {
			t.Errorf("config import printed the notice: %q", stderr)
		}
		if !strings.Contains(stdout, "saved from") {
			t.Errorf("config import output = %q, want a saved version", stdout)
		}
	})
}

func TestConfigFlagDeprecationNoticeOnSubcommands(t *testing.T) {
	const notice = "Warning: --config is deprecated. Use 'rabbithole config import FILE' instead."

	e := newCLIEnv(t)
	cfg := e.write("a.yaml", goodConfig)

	for _, args := range [][]string{
		{"items", "list"},
		{"sources", "export"},
		{"config", "history"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stderr, err := e.run(append(args, "--config", cfg)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(stderr, notice); got != 1 {
				t.Fatalf("stderr has %d notices, want 1:\n%s", got, stderr)
			}
		})
	}
}

func TestConfigFlagDeprecationNoticeOnBenchmark(t *testing.T) {
	const notice = "Warning: --config is deprecated. Use 'rabbithole config import FILE' instead."
	const golden = `metadata:
  name: deprecation-test
  version: 1
tags:
  on-target: relevant
samples:
  - id: one
    source: Source
    title: A relevant article
    summary: Enough text to score.
    expected_llm_score: 8
    tags: [on-target]
`

	resetBenchmarkFlags()
	t.Cleanup(resetBenchmarkFlags)

	e := newCLIEnv(t)
	cfg := e.write("a.yaml", goodConfig)
	report := e.path("report.json")

	_, stderr, err := e.run(
		"eval", "benchmark", e.write("golden.yaml", golden),
		"--provider", heuristicProvider,
		"--repeats", "1",
		"--format", "json",
		"--output-path", report,
		"--config", cfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(stderr, notice); got != 1 {
		t.Fatalf("stderr has %d notices, want 1:\n%s", got, stderr)
	}
	if !fileExists(report) {
		t.Error("benchmark did not write its report")
	}
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

func TestConfigExportForms(t *testing.T) {
	const saved = "inference:\n  provider: vllm\n  host: http://gpu:8000\n  model: m1\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n"
	tests := []struct {
		name  string
		saved bool
		args  []string
		check func(t *testing.T, out string)
	}{
		{"empty store yaml", false, []string{"export"}, func(t *testing.T, out string) {
			if !strings.HasPrefix(out, "# rabbithole: no config is saved") || strings.Count(out, "\n") != 1 {
				t.Errorf("out = %q", out)
			}
		}},
		{"empty store json", false, []string{"export", "--format", "json"}, func(t *testing.T, out string) {
			if out != "{}\n" {
				t.Errorf("out = %q", out)
			}
		}},
		{"empty store defaults yaml", false, []string{"export", "--defaults"}, func(t *testing.T, out string) {
			for _, want := range []string{"# rabbithole: every setting", "provider: ollama", "host: http://localhost:11434",
				"think: true", "batch_size: 1", "tokens_per_item: 1024", "system_prompt:", "user:"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			if strings.Contains(out, "effort") || strings.Contains(out, "api_key") {
				t.Errorf("ollama reads neither effort nor api_key in a default export:\n%s", out)
			}
		}},
		{
			"empty store defaults json",
			false,
			[]string{"export", "--defaults", "-f", "json"},
			func(t *testing.T, out string) {
				var got struct {
					Inference struct {
						Provider    string `json:"provider"`
						BatchSize   int    `json:"batch_size"`
						ModelTuning struct {
							Per int `json:"tokens_per_item"`
						} `json:"model_tuning"`
					} `json:"inference"`
				}
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("not json: %v\n%s", err, out)
				}
				if got.Inference.Provider != "ollama" || got.Inference.BatchSize != 1 ||
					got.Inference.ModelTuning.Per != 1024 {
					t.Errorf("got %+v", got)
				}
			},
		},
		{"saved yaml", true, []string{"export"}, func(t *testing.T, out string) {
			if !strings.HasPrefix(out, "# rabbithole config version 1 (import:") ||
				!strings.Contains(out, "model: m1") ||
				strings.Contains(out, "batch_size") {
				t.Errorf("out:\n%s", out)
			}
		}},
		{"saved json", true, []string{"export", "-f", "json"}, func(t *testing.T, out string) {
			want := "{\n  \"inference\": {\n    \"provider\": \"vllm\",\n    \"host\": \"http://gpu:8000\",\n" +
				"    \"model\": \"m1\",\n    \"api_key\": \"$RABBITHOLE_INFERENCE_API_KEY\"\n  }\n}\n"
			if out != want {
				t.Errorf("out:\n%s\nwant:\n%s", out, want)
			}
		}},
		{
			"json inferred from the file name",
			true,
			[]string{"export", "-o", "OUT.json"},
			func(t *testing.T, out string) {
				if !strings.HasPrefix(out, "{\n") {
					t.Errorf("out:\n%s", out)
				}
			},
		},
		{
			"format flag beats the file name",
			true,
			[]string{"export", "-f", "yaml", "-o", "OUT.json"},
			func(t *testing.T, out string) {
				if !strings.HasPrefix(out, "# rabbithole config version") {
					t.Errorf("out:\n%s", out)
				}
			},
		},
		{"saved defaults yaml", true, []string{"export", "--defaults"}, func(t *testing.T, out string) {
			for _, want := range []string{"# rabbithole config version 1 (import:", "stays a default", "provider: vllm",
				"model: m1", "api_key: $RABBITHOLE_INFERENCE_API_KEY", "think: true", "batch_size: 1", "structured_output: true"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			if strings.Contains(out, "num_ctx") || strings.Contains(out, "effort") {
				t.Errorf("vllm reads neither num_ctx nor effort:\n%s", out)
			}
		}},
		{"saved defaults json", true, []string{"export", "--defaults", "-f", "json"}, func(t *testing.T, out string) {
			if strings.HasPrefix(out, "#") || !json.Valid([]byte(out)) ||
				!strings.Contains(
					out,
					`"api_key": "$RABBITHOLE_INFERENCE_API_KEY"`,
				) || !strings.Contains(out, `"batch_size": 1`) {
				t.Errorf("out:\n%s", out)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newCLIEnv(t)
			if tc.saved {
				e.mustRun("config", "import", e.write("a.yaml", saved))
			}
			args := slices.Clone(tc.args)
			for i, a := range args {
				if a == "OUT.json" {
					args[i] = e.path("out.json")
				}
			}
			out := e.mustRun(append([]string{"config"}, args...)...)
			if i := slices.Index(args, "-o"); i >= 0 {
				raw, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatal(err)
				}
				out = string(raw)
			}
			tc.check(t, out)
		})
	}
	e := newCLIEnv(t)
	if _, _, err := e.run(
		"config",
		"export",
		"-f",
		"toml",
	); err == nil ||
		!strings.Contains(err.Error(), "yaml or json") {
		t.Errorf("unknown format error = %v", err)
	}
}

func TestConfigJSONExportRoundTripsToTheSameSparseConfig(t *testing.T) {
	e := newCLIEnv(t)
	prompt := "Score each item.\nBe terse."
	e.mustRun(
		"config",
		"import",
		e.write("a.yaml", goodConfig+"  system_prompt: |-\n    Score each item.\n    Be terse.\n  batch_size: 4\n"),
	)
	want := e.mustRun("config", "export", "-f", "json")
	if !strings.Contains(want, `"system_prompt": "`+strings.ReplaceAll(prompt, "\n", `\n`)+`"`) {
		t.Fatalf("json lacks the prompt:\n%s", want)
	}
	other := newCLIEnv(t)
	out := other.mustRun("config", "import", other.write("a.json", want))
	if !strings.Contains(out, "first saved version") {
		t.Errorf("import = %q", out)
	}
	if got := other.mustRun("config", "export", "-f", "json"); got != want {
		t.Errorf("round trip changed the config:\n%s\nwant:\n%s", got, want)
	}
	if out := other.mustRun(
		"config",
		"import",
		other.write("b.json", want),
	); !strings.Contains(
		out,
		"config unchanged",
	) {
		t.Errorf("reimport = %q", out)
	}
}

func TestConfigDefaultsExportImportsAsNothingPinned(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			// The default store: the full export pins nothing and changes nothing.
			e := newCLIEnv(t)
			full := e.write("full."+format, e.mustRun("config", "export", "--defaults", "-f", format))
			out := e.mustRun("config", "import", full)
			if !strings.Contains(out, "equal the default and stay defaults") ||
				!strings.Contains(out, "first saved version") {
				t.Errorf("import = %q", out)
			}
			if got := e.mustRun("config", "export", "-f", "json"); got != "{}\n" {
				t.Errorf("sparse config after import = %q, want {}", got)
			}
			if out := e.mustRun("config", "import", full); !strings.Contains(out, "config unchanged") {
				t.Errorf("second import = %q", out)
			}

			// A store with changes: moving it through the full export keeps only them.
			a := newCLIEnv(t)
			a.mustRun("config", "import", a.write("a.yaml", goodConfig))
			want := a.mustRun("config", "export", "-f", "json")
			full = a.write("full."+format, a.mustRun("config", "export", "--defaults", "-f", format))
			b := newCLIEnv(t)
			b.mustRun("config", "import", full)
			if got := b.mustRun("config", "export", "-f", "json"); got != want {
				t.Errorf("sparse config = %s, want %s", got, want)
			}
		})
	}
}

func TestConfigImportReportsValuesEqualToTheDefault(t *testing.T) {
	e := newCLIEnv(t)
	file := e.write("a.yaml", "inference:\n  provider: ollama\n  think: true\n  batch_size: 1\n  model: other\n")
	out := e.mustRun("config", "import", file)
	if !strings.Contains(out, "3 values equal the default and stay defaults") {
		t.Errorf("import = %q", out)
	}
	exported := e.mustRun("config", "export")
	if !strings.Contains(exported, "model: other") || strings.Contains(exported, "think") ||
		strings.Contains(exported, "provider") || strings.Contains(exported, "batch_size") {
		t.Errorf("export keeps more than the change:\n%s", exported)
	}
	one := e.write("b.yaml", "inference:\n  provider: ollama\n  model: third\n")
	if out := e.mustRun(
		"config",
		"import",
		one,
	); !strings.Contains(
		out,
		"1 value equals the default and stays a default",
	) {
		t.Errorf("import = %q", out)
	}
	plain := e.write("c.yaml", "inference:\n  model: fourth\n")
	if out := e.mustRun("config", "import", plain); strings.Contains(out, "default") {
		t.Errorf("import with no defaults mentions them: %q", out)
	}
}

func TestConfigExportFormsNeverPrintASecret(t *testing.T) {
	e := newCLIEnv(t)
	const literal = "sk-literal-abc123XYZ"
	e.mustRun("config", "import", e.write("a.yaml", goodConfig+"  api_key: "+literal+"\n"))
	for _, args := range [][]string{
		{"export"}, {"export", "-f", "json"}, {"export", "--defaults"}, {"export", "--defaults", "-f", "json"},
	} {
		out := e.mustRun(append([]string{"config"}, args...)...)
		if strings.Contains(out, literal) || !strings.Contains(out, "$RABBITHOLE_INFERENCE_API_KEY") {
			t.Errorf("%v:\n%s", args, out)
		}
	}
}
