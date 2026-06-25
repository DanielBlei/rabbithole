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

	w := New(db, &config.Config{}, ":8080", path)
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
	w := New(db, &config.Config{}, ":8080", filepath.Join(t.TempDir(), "nope.yaml"))
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
