package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

func TestHandleConfig(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "provider: ollama\nthink: false # no thinking mode\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	w := New(db, &config.Config{}, ":8080", path, testIngestManager(t, db))
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config: status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	if !strings.Contains(out, path) {
		t.Errorf("config modal missing path %q", path)
	}
	if !strings.Contains(out, `<span class="yml-key">provider</span>`) {
		t.Errorf("config modal missing highlighted key; body=%s", out)
	}
}

func TestHandleConfigMissingFile(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// A bad path renders the modal with an error rather than failing the request.
	w := New(db, &config.Config{}, ":8080", filepath.Join(t.TempDir(), "nope.yaml"), testIngestManager(t, db))
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "could not read config") {
		t.Errorf("expected error message in modal; body=%s", rec.Body)
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
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const secret = "sk-live-SUPERSECRET-0123456789"
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "inference:\n  provider: ollama\n  api_key: \"" + secret + "\" # ollama cloud\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	w := New(db, &config.Config{}, ":8080", path, testIngestManager(t, db))
	rec := httptest.NewRecorder()
	w.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config: status = %d, want 200", rec.Code)
	}
	out := rec.Body.String()
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

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"quoted key", `  api_key: "abc123"`, "  api_key: " + redactedMask},
		{"bare key", `api_key: abc123`, "api_key: " + redactedMask},
		{"prefixed key", `  openai_api_key: abc`, "  openai_api_key: " + redactedMask},
		{"token", `token: xyz`, "token: " + redactedMask},
		{"auth_token", `  auth_token: xyz`, "  auth_token: " + redactedMask},
		{"password", `password: hunter2`, "password: " + redactedMask},
		{"client_secret", `client_secret: shh`, "client_secret: " + redactedMask},
		{"list item", `  - token: xyz`, "  - token: " + redactedMask},
		{"comment kept", `api_key: abc # note`, "api_key: " + redactedMask + " # note"},
		// Nothing to hide: an unset value stays legible.
		{"empty quoted", `api_key: ""`, `api_key: ""`},
		{"empty", `api_key:`, `api_key:`},
		{"null", `api_key: null`, `api_key: null`},
		// Non-secret keys are untouched, including ones merely mentioning it.
		{"other key", `model: qwen3:4b`, `model: qwen3:4b`},
		{"comment only", `# set api_key here`, `# set api_key here`},
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
