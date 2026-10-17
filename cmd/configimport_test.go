// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// importEnv is one import's world: a store, an env file, and a config directory.
type importEnv struct {
	t   *testing.T
	db  *store.Store
	env *config.EnvFile
	dir string
	log *bytes.Buffer
}

// clearVar leaves name unset for the test and restores it afterwards.
func clearVar(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	for _, name := range []string{"RABBITHOLE_INFERENCE_API_KEY", "RABBITHOLE_SUMMARY_API_KEY"} {
		clearVar(t, name)
	}
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	env, err := config.LoadEnvFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	old := log
	log = zerolog.New(buf)
	t.Cleanup(func() { log = old })
	return &importEnv{t: t, db: db, env: env, dir: dir, log: buf}
}

func (e *importEnv) write(name, body string) string {
	e.t.Helper()
	path := filepath.Join(e.dir, name)
	writeFile(e.t, path, body)
	return path
}

func (e *importEnv) do(path string, opts importOpts) (importOutcome, error) {
	e.t.Helper()
	return importConfig(e.t.Context(), e.db, e.env, path, opts)
}

func (e *importEnv) active() (store.ConfigVersion, bool) {
	e.t.Helper()
	v, ok, err := e.db.ActiveConfig(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	return v, ok
}

func (e *importEnv) envFileText() string {
	raw, err := os.ReadFile(e.env.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return string(raw)
}

const goodConfig = "inference:\n  provider: vllm\n  host: http://gpu:8000\n  model: m1\n"

func TestImportMovesLiteralKeyToEnvFile(t *testing.T) {
	e := newImportEnv(t)
	const literal = "sk-literal-abc123XYZ"
	path := e.write("c.yaml", goodConfig+"  api_key: "+literal+"\n")

	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved || out.Version != 1 {
		t.Fatalf("import = %+v, %v; want saved version 1", out, err)
	}
	v, _ := e.active()
	if !strings.Contains(v.Content, "api_key: $RABBITHOLE_INFERENCE_API_KEY") {
		t.Errorf("stored content = %q, want the reference", v.Content)
	}
	if strings.Contains(v.Content, literal) {
		t.Errorf("the literal reached the store: %q", v.Content)
	}
	if got := e.envFileText(); got != "RABBITHOLE_INFERENCE_API_KEY="+literal+"\n" {
		t.Errorf("env file = %q", got)
	}
	if os.Getenv("RABBITHOLE_INFERENCE_API_KEY") != literal {
		t.Error("the next run in this process does not see the key")
	}
	if v.Source != store.ConfigSourceImport+path {
		t.Errorf("source = %q", v.Source)
	}
}

func TestImportRefusesNonAllowListedReference(t *testing.T) {
	e := newImportEnv(t)
	for _, ref := range []string{"$AWS_SECRET_ACCESS_KEY", "$RABBITHOLE_DB_PASSWORD", "$RABBITHOLE_DB", "$RABBITHOLE_X"} {
		path := e.write("c.yaml", goodConfig+"  api_key: "+ref+"\n")
		if _, err := e.do(path, importOpts{}); err == nil {
			t.Fatalf("%s was accepted", ref)
		}
		if _, ok := e.active(); ok {
			t.Fatalf("%s: a version was saved", ref)
		}
	}
	if e.envFileText() != "" {
		t.Error("the env file was written for a refused import")
	}
}

func TestImportValidationFailureKeepsActive(t *testing.T) {
	e := newImportEnv(t)
	if _, err := e.do(e.write("good.yaml", goodConfig), importOpts{}); err != nil {
		t.Fatal(err)
	}
	bad := e.write("bad.yaml", "inference:\n  provider: openai\n  effort: max\n  api_key: sk-secret-1234\n")
	_, err := e.do(bad, importOpts{})
	if err == nil || !strings.Contains(err.Error(), "inference.effort") {
		t.Fatalf("err = %v, want the field error", err)
	}
	v, _ := e.active()
	if v.Version != 1 {
		t.Errorf("active version = %d, want 1", v.Version)
	}
	if e.envFileText() != "" {
		t.Error("a rejected import wrote its key to the env file")
	}
}

func TestImportWarnsAboutDroppedKeys(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", "inference:\n  provider: ollama\n  effort: high\n  model: m\n")
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	logged := e.log.String()
	if !strings.Contains(logged, "ignored for provider ollama: inference.effort") {
		t.Errorf("no dropped-key warning in %q", logged)
	}
}

func TestImportSeedsLegacyFeedsOnce(t *testing.T) {
	e := newImportEnv(t)
	feeds := e.write("feeds.yaml", "feeds:\n  - name: A\n    url: http://a.test/feed\n")
	path := e.write("c.yaml", goodConfig+"ingest:\n  feeds: "+feeds+"\n")

	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved {
		t.Fatalf("first import = %+v, %v", out, err)
	}
	count := func() int {
		doc, err := e.db.FeedsDoc(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return len(doc.Feeds)
	}
	if count() != 1 {
		t.Fatalf("feeds after import = %d, want 1", count())
	}

	writeFile(t, feeds, "feeds:\n  - name: A\n    url: http://a.test/feed\n  - name: B\n    url: http://b.test/feed\n")
	e.log.Reset()
	out, err = e.do(path, importOpts{})
	if err != nil || out.Saved {
		t.Fatalf("second import = %+v, %v; want a noop", out, err)
	}
	if count() != 1 {
		t.Errorf("feeds after the noop = %d, want 1: the seed file is read only on import", count())
	}
	if !strings.Contains(e.log.String(), "ingest.feeds") {
		t.Errorf("no note that ingest.feeds is import-only: %q", e.log.String())
	}
}

func TestImportSeedsLegacyProfile(t *testing.T) {
	e := newImportEnv(t)
	profile := e.write("profile.md", "# Profile\n- local models\n")
	path := e.write("c.yaml", goodConfig+"profile: "+profile+"\n")
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	id, set, err := e.db.ActiveProfileID(t.Context())
	if err != nil || !set || id == "" {
		t.Fatalf("active profile = %q, %v, %v; want the imported one", id, set, err)
	}
}

func TestImportFileOwnedConfigUpdatesSilently(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", goodConfig)
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, goodConfig+"user: ada\n")
	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved || out.Version != 2 {
		t.Fatalf("edit = %+v, %v; want version 2", out, err)
	}
	// Back to the original text, which equals version 1: the file still owns
	// the config, so it applies.
	writeFile(t, path, goodConfig)
	out, err = e.do(path, importOpts{})
	if err != nil || !out.Saved || out.Version != 3 {
		t.Fatalf("revert = %+v, %v; want version 3", out, err)
	}
	if !strings.Contains(e.log.String(), "config updated from") || out.Replaced != nil {
		t.Errorf("want the update line and no replaced warning: %q", e.log.String())
	}
}

// A rerun of the same file saves nothing.
func TestImportRerunIsNoop(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", goodConfig)
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	out, err := e.do(path, importOpts{})
	if err != nil || out.Saved || out.Version != 1 {
		t.Fatalf("rerun = %+v, %v; want a noop on version 1", out, err)
	}
}

// After a save from the UI, the unchanged file is applied again and the
// replaced version is named with the undo command.
func TestImportReappliesFileOverUISave(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", goodConfig)
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.SaveConfig(t.Context(), "user: bob\n", "ui-hash", store.ConfigSourceUI, 1); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved || out.Version != 3 || out.Previous != 2 {
		t.Fatalf("import = %+v, %v; want version 3 replacing 2", out, err)
	}
	if out.Replaced == nil || out.Replaced.Version != 2 || out.Replaced.Source != store.ConfigSourceUI {
		t.Errorf("Replaced = %+v, want the UI version 2", out.Replaced)
	}
}

// Another file's import is replaced the same way.
func TestImportReplacesAnotherFilesConfig(t *testing.T) {
	e := newImportEnv(t)
	if _, err := e.do(e.write("a.yaml", goodConfig), importOpts{}); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	b := e.write("b.yaml", goodConfig+"user: ada\n")
	out, err := e.do(b, importOpts{})
	if err != nil || !out.Saved || out.Version != 2 {
		t.Fatalf("import = %+v, %v; want version 2", out, err)
	}
	if out.Replaced == nil || out.Replaced.Version != 1 {
		t.Errorf("Replaced = %+v, want version 1", out.Replaced)
	}
}

// Without ingest.feeds, a feeds.yaml beside the config file is not read.
func TestImportDoesNotReadSiblingFeeds(t *testing.T) {
	e := newImportEnv(t)
	e.write("feeds.yaml", "feeds:\n  - name: A\n    url: http://a.test/feed\n")
	path := e.write("c.yaml", goodConfig)
	if out, err := e.do(path, importOpts{}); err != nil || !out.Saved {
		t.Fatalf("import = %+v, %v", out, err)
	}
	doc, err := e.db.FeedsDoc(t.Context())
	if err != nil || len(doc.Feeds) != 0 {
		t.Fatalf("feeds = %+v, %v; want none", doc.Feeds, err)
	}
}

func TestImportRotatesKeyOnNoop(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", goodConfig+"  api_key: sk-first-1111\n")
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, goodConfig+"  api_key: sk-second-2222\n")
	out, err := e.do(path, importOpts{})
	if err != nil || out.Saved {
		t.Fatalf("import = %+v, %v; want a noop", out, err)
	}
	if got := e.envFileText(); got != "RABBITHOLE_INFERENCE_API_KEY=sk-second-2222\n" {
		t.Errorf("env file = %q, want the rotated key", got)
	}
}

func TestImportEnvironmentWinsOverFileLiteral(t *testing.T) {
	e := newImportEnv(t)
	t.Setenv("RABBITHOLE_INFERENCE_API_KEY", "from-the-environment")
	path := e.write("c.yaml", goodConfig+"  api_key: sk-from-file-3333\n")
	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved {
		t.Fatalf("import = %+v, %v", out, err)
	}
	if e.envFileText() != "" {
		t.Error("the env file was written although the environment sets the variable")
	}
	if os.Getenv("RABBITHOLE_INFERENCE_API_KEY") != "from-the-environment" {
		t.Error("the environment value was replaced")
	}
	if !strings.Contains(e.log.String(), "RABBITHOLE_INFERENCE_API_KEY is set in the environment") {
		t.Errorf("no ignored-literal warning in %q", e.log.String())
	}
}

func TestImportNeverLogsTheKey(t *testing.T) {
	e := newImportEnv(t)
	log = zerolog.New(e.log).Level(zerolog.TraceLevel)

	const literal = "sk-LOGLEAKCHECK-0987654321"
	path := e.write("c.yaml", goodConfig+"  api_key: "+literal+"\n  effort: high\n")
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, goodConfig+"  api_key: "+literal+"x\n")
	if _, err := e.do(path, importOpts{}); err != nil { // rotation on a noop
		t.Fatal(err)
	}
	bad := e.write("bad.yaml", "inference:\n  provider: nope\n  api_key: "+literal+"\n")
	_, err := e.do(bad, importOpts{})
	if err == nil {
		t.Fatal("want an error for the bad provider")
	}
	if strings.Contains(e.log.String(), literal) || strings.Contains(err.Error(), literal) {
		t.Fatalf("the api_key value appears in the output:\nlog: %s\nerr: %v", e.log.String(), err)
	}
	if e.log.Len() == 0 {
		t.Fatal("nothing was logged; the check proves nothing")
	}
}

func TestImportBadProfileSavesNothing(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", goodConfig+"profile: "+filepath.Join(e.dir, "missing.md")+"\n  \n")
	_, err := e.do(path, importOpts{})
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, ok := e.active(); ok {
		t.Error("a version was saved despite the unreadable profile")
	}
}

func TestImportBadFeedsFileOnlyWarns(t *testing.T) {
	e := newImportEnv(t)
	feeds := e.write("feeds.yaml", "feeds:\n  - name: [broken\n")
	path := e.write("c.yaml", goodConfig+"ingest:\n  feeds: "+feeds+"\n")
	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved {
		t.Fatalf("import = %+v, %v; want it saved", out, err)
	}
	if !strings.Contains(e.log.String(), "reading the feed seed file failed") {
		t.Errorf("no feeds warning in %q", e.log.String())
	}
}

func TestImportConflictRetrySavesOnce(t *testing.T) {
	e := newImportEnv(t)
	if _, err := e.do(e.write("first.yaml", goodConfig), importOpts{}); err != nil {
		t.Fatal(err)
	}
	other := e.write("second.yaml", goodConfig+"user: ada\n")
	opts := importOpts{}
	raced := false
	opts.beforeSave = func() {
		if raced {
			return
		}
		raced = true
		if _, err := e.db.SaveConfig(t.Context(), "user: racer\n", "racer", store.ConfigSourceUI, 1); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.do(other, opts)
	if err != nil || !out.Saved || out.Version != 3 {
		t.Fatalf("import = %+v, %v; want version 3 after one retry", out, err)
	}
}

func TestImportReplacesUnreadableRow(t *testing.T) {
	e := newImportEnv(t)
	if _, err := e.db.SaveConfig(t.Context(), "bogus_key: 1\n", "bad", store.ConfigSourceUI, 0); err != nil {
		t.Fatal(err)
	}
	path := e.write("c.yaml", goodConfig)
	out, err := e.do(path, importOpts{})
	if err != nil || !out.Saved || out.Version != 2 {
		t.Fatalf("import = %+v, %v; want version 2", out, err)
	}
	if _, _, err := e.db.ConfigSource().Active(t.Context()); err != nil {
		t.Errorf("the fixed config still does not read: %v", err)
	}
}

func TestImportSymlinkIsTheSameOwner(t *testing.T) {
	e := newImportEnv(t)
	path := e.write("c.yaml", goodConfig)
	link := filepath.Join(e.dir, "link.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := e.do(path, importOpts{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, goodConfig+"user: ada\n")
	out, err := e.do(link, importOpts{})
	if err != nil || !out.Saved || out.Replaced != nil {
		t.Fatalf("import via the link = %+v, %v; want the owner's silent update: %q", out, err, e.log.String())
	}
}
