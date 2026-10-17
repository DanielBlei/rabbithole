// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/store"
)

func configTestWeb(t *testing.T) (*Web, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, db.ConfigSource(), testIngestManager(t, db)), db
}

func getConfig(t *testing.T, w *Web) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config: status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestHandleConfigShowsActiveVersion(t *testing.T) {
	w, db := configTestWeb(t)
	ctx := t.Context()
	if _, err := db.SaveConfig(ctx, "inference:\n  provider: ollama\n", "h1", "import:/a/c.yaml", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveConfig(
		ctx,
		"inference:\n  provider: vllm # served locally\n",
		"h2",
		store.ConfigSourceUI,
		1,
	); err != nil {
		t.Fatal(err)
	}
	out := getConfig(t, w)
	if !strings.Contains(out, "config version 2") || !strings.Contains(out, "ui") {
		t.Errorf("title missing the active version and source; body=%s", out)
	}
	if !strings.Contains(out, `<span class="yml-key">provider</span>`) || !strings.Contains(out, "vllm") {
		t.Errorf("config modal missing the active content; body=%s", out)
	}
	if strings.Contains(out, "import:/a/c.yaml") {
		t.Errorf("an older version's source leaked into the title; body=%s", out)
	}
}

func TestHandleConfigDefaultsWhenNothingSaved(t *testing.T) {
	w, _ := configTestWeb(t)
	out := getConfig(t, w)
	if !strings.Contains(out, "defaults") || !strings.Contains(out, "nothing saved yet") {
		t.Errorf("expected the defaults note; body=%s", out)
	}
}

func TestHighlightYAML(t *testing.T) {
	got := string(highlightYAML("provider: ollama # backend\nthink: false\nbatch_size: 5\nurl: \"http://x#y\""))

	for _, want := range []string{
		`<span class="yml-key">provider</span>`,
		`<span class="yml-comment"># backend</span>`,
		`<span class="yml-bool">false</span>`,
		`<span class="yml-num">5</span>`,
		`<span class="yml-string">&#34;http://x#y&#34;</span>`, // '#' inside a quoted value is not a comment; quotes are HTML-escaped
	} {
		if !strings.Contains(got, want) {
			t.Errorf("highlightYAML output missing %q\ngot: %s", want, got)
		}
	}
}

// A set credential must never reach the rendered page — the modal is opened
// over screen-shares and lands in screenshots (review finding 1.1).
func TestHandleConfigRedactsSecrets(t *testing.T) {
	w, db := configTestWeb(t)

	const secret = "sk-live-SUPERSECRET-0123456789"
	body := "inference:\n  provider: ollama\n  api_key: \"" + secret + "\" # ollama cloud\n"
	if _, err := db.SaveConfig(t.Context(), body, "h", store.ConfigSourceUI, 0); err != nil {
		t.Fatal(err)
	}
	out := getConfig(t, w)
	if strings.Contains(out, secret) {
		t.Fatalf("api_key leaked into the config modal; body=%s", out)
	}
	// The field and its comment still show, so the viewer stays informative.
	if !strings.Contains(out, "api_key") || !strings.Contains(out, redactedMask) {
		t.Errorf("expected a masked api_key field; body=%s", out)
	}
	if !strings.Contains(out, "ollama cloud") {
		t.Errorf("trailing comment should survive redaction; body=%s", out)
	}
}

func TestHandleConfigKeepsAllowedReferences(t *testing.T) {
	w, db := configTestWeb(t)
	body := "inference:\n  provider: ollama\n  api_key: $RABBITHOLE_INFERENCE_API_KEY\n"
	if _, err := db.SaveConfig(t.Context(), body, "h", store.ConfigSourceUI, 0); err != nil {
		t.Fatal(err)
	}
	out := getConfig(t, w)
	if !strings.Contains(out, "RABBITHOLE_INFERENCE_API_KEY") {
		t.Errorf("allow-listed reference was masked; body=%s", out)
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"quoted key", `  api_key: "abc123"`, "  api_key: " + redactedMask},
		{"bare key", `api_key: abc123`, "api_key: " + redactedMask},
		{"prefixed key", `  openai_api_key: abc`, "  openai_api_key: " + redactedMask},
		{"tokens_per_item stays visible", `    tokens_per_item: 400`, `    tokens_per_item: 400`},
		{"tokens_overhead stays visible", `    tokens_overhead: 200`, `    tokens_overhead: 200`},
		{"tokens_thinking stays visible", `    tokens_thinking: 800`, `    tokens_thinking: 800`},
		{"max_tokens stays visible", `    max_tokens: 800`, `    max_tokens: 800`},
		{"dashed key", `  api-token: xyz`, "  api-token: " + redactedMask},
		{"token", `token: xyz`, "token: " + redactedMask},
		{"auth_token", `  auth_token: xyz`, "  auth_token: " + redactedMask},
		{"password", `password: hunter2`, "password: " + redactedMask},
		{"client_secret", `client_secret: shh`, "client_secret: " + redactedMask},
		{"list item", `  - token: xyz`, "  - token: " + redactedMask},
		{"comment kept", `api_key: abc # note`, "api_key: " + redactedMask + " # note"},
		{"env reference outside the allow-list", `api_key: $API_KEY`, "api_key: " + redactedMask},
		{"allow-listed reference", `api_key: $RABBITHOLE_INFERENCE_API_KEY`, `api_key: $RABBITHOLE_INFERENCE_API_KEY`},
		{
			"braced allow-listed reference",
			`api_key: ${RABBITHOLE_SUMMARY_API_KEY}`,
			`api_key: ${RABBITHOLE_SUMMARY_API_KEY}`,
		},
		{"db url reference", `api_key: $RABBITHOLE_DB`, "api_key: " + redactedMask},
		{"db password reference", `api_key: $RABBITHOLE_DB_PASSWORD`, "api_key: " + redactedMask},
		// Nothing to hide: an unset value stays legible.
		{"empty quoted", `api_key: ""`, `api_key: ""`},
		{"empty", `api_key:`, `api_key:`},
		{"null", `api_key: null`, `api_key: null`},
		// Non-secret keys are untouched, including ones merely mentioning it.
		{"other key", `model: qwen3:4b`, `model: qwen3:4b`},
		{"comment only", `# set api_key here`, `# set api_key here`},
		// A password inside a value: the key says nothing, so only the URL's
		// own shape gives it away. store.url is the one that matters.
		{
			"url password",
			`  url: postgres://rabbit@db.host:5432/rabbithole`,
			`  url: postgres://rabbit@db.host:5432/rabbithole`,
		},
		// No userinfo at all: the `:` here belongs to the port, and a redactor
		// that read it as the user/password split would mask the port away.
		{
			"host and port only, nothing to redact",
			`  url: postgres://db.host:5432/rabbithole`,
			`  url: postgres://db.host:5432/rabbithole`,
		},
		// A Postgres password names the variable it belongs in rather than
		// showing dots, so the viewer is one more place that says where to
		// put it.
		{
			"url with password",
			`  url: postgres://rabbit:hunter2@db.host:5432/rabbithole`,
			`  url: postgres://rabbit:` + dbPasswordHint + `@db.host:5432/rabbithole`,
		},
		{
			"url with password and query",
			`url: postgresql://u:p@h/db?sslmode=verify-full`,
			`url: postgresql://u:` + dbPasswordHint + `@h/db?sslmode=verify-full`,
		},
		// libpq takes the password as a query parameter too, which the
		// userinfo pattern does not see.
		{
			"password as a query parameter",
			`  url: postgres://rabbit@db.host/rabbithole?password=hunter2&sslmode=require`,
			`  url: postgres://rabbit@db.host/rabbithole?password=` + dbPasswordHint + `&sslmode=require`,
		},
		// The password may itself contain an `@`, and the authority's separator is
		// the *last* one — so redaction has to run through it, not stop at the
		// first character that looks like one.
		{
			"password containing an at sign",
			`  url: postgres://rabbit:p@ss@db.host/rabbithole`,
			`  url: postgres://rabbit:` + dbPasswordHint + `@db.host/rabbithole`,
		},
		// ResolvePostgres reads the parameter through url.Query, which decodes
		// keys, so an encoded key names a password just as well as a plain one.
		{
			"password parameter with an encoded key",
			`  url: postgres://rabbit@db.host/rabbithole?pass%77ord=hunter2&sslmode=require`,
			`  url: postgres://rabbit@db.host/rabbithole?pass%77ord=` + dbPasswordHint + `&sslmode=require`,
		},
		{
			"env reference in userinfo stays visible",
			`url: postgres://rabbit:$PG_PASS@h/db`,
			`url: postgres://rabbit:$PG_PASS@h/db`,
		},
		{
			"env reference as a query parameter stays visible",
			`url: postgres://rabbit@h/db?password=${PG_PASS}&sslmode=require`,
			`url: postgres://rabbit@h/db?password=${PG_PASS}&sslmode=require`,
		},
		{
			"literal dollar password in userinfo is masked",
			`endpoint: https://user:$ecret@example.com/v1`,
			`endpoint: https://user:` + redactedMask + `@example.com/v1`,
		},
		{
			"literal dollar password parameter is masked",
			`url: postgres://rabbit@h/db?password=$3cret!&sslmode=require`,
			`url: postgres://rabbit@h/db?password=` + dbPasswordHint + `&sslmode=require`,
		},
		{
			"password parameter not first",
			`url: postgres://rabbit@h/db?sslmode=require&password=hunter2`,
			`url: postgres://rabbit@h/db?sslmode=require&password=` + dbPasswordHint,
		},
		// A non-Postgres URL keeps the generic mask: the variable is the
		// store's, and naming it here would be a lie.
		{
			"other scheme keeps the plain mask",
			`endpoint: https://user:hunter2@example.com/v1`,
			`endpoint: https://user:` + redactedMask + `@example.com/v1`,
		},
		// A bare host:port is not credentials and must stay readable.
		{"host and port", `host: http://localhost:11434`, `host: http://localhost:11434`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactSecrets(c.in); got != c.want {
				t.Errorf("redactSecrets(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Redaction must survive a multi-line file without disturbing its neighbours.
func TestRedactSecretsMultiline(t *testing.T) {
	in := "inference:\n  provider: ollama\n  api_key: secret-value\n  model: qwen3:4b\n"
	got := redactSecrets(in)
	if strings.Contains(got, "secret-value") {
		t.Errorf("secret survived redaction: %q", got)
	}
	for _, want := range []string{"provider: ollama", "model: qwen3:4b", "api_key: " + redactedMask} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got %q", want, got)
		}
	}
}

func TestHandleConfigManagedNote(t *testing.T) {
	w, _ := configTestWeb(t)
	if out := getConfig(t, w); strings.Contains(out, "is managed by") {
		t.Errorf("managed note shown without --config; body=%s", out)
	}
	w.SetManagedBy("./a.yaml")
	out := getConfig(t, w)
	if !strings.Contains(out, "config is managed by") || !strings.Contains(out, "./a.yaml") ||
		!strings.Contains(out, "re-applied on every start") {
		t.Errorf("managed note missing; body=%s", out)
	}
}
