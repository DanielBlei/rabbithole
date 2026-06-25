package ollama

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/ai-searcher/internal/feeds"
)

func withFastValidateRetry(t *testing.T) {
	origAttempts, origBackoff := validateAttempts, validateBackoff
	validateAttempts = 3
	validateBackoff = time.Millisecond
	t.Cleanup(func() {
		validateAttempts = origAttempts
		validateBackoff = origBackoff
	})
}

func TestValidate(t *testing.T) {
	withFastValidateRetry(t)

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "model present succeeds", body: `{"models":[{"model":"llama3:latest"}]}`},
		{name: "model missing fails", body: `{"models":[{"model":"other:latest"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3:latest", "", false)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = c.Validate(t.Context())
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want non-nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestValidateThinkProbe(t *testing.T) {
	withFastValidateRetry(t)

	const listBody = `{"models":[{"model":"llama3:latest"}]}`

	tests := []struct {
		name      string
		chatErr   string // chat-endpoint error body; empty means the model accepts think
		wantThink bool
	}{
		{name: "supported model keeps think on", wantThink: true},
		{name: "unsupported model falls back to think off", chatErr: `"llama3:latest" does not support thinking`},
		{name: "other chat error leaves think on", chatErr: "some transient failure", wantThink: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/api/chat") {
					if tt.chatErr != "" {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":` + strconv.Quote(tt.chatErr) + `}`))
						return
					}
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"hi"},"done":true}` + "\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(listBody))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3:latest", "", true)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if err := c.Validate(t.Context()); err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if c.think != tt.wantThink {
				t.Fatalf("think = %v after Validate, want %v", c.think, tt.wantThink)
			}
		})
	}
}

func TestListModelNames(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		statusCode int // 0 means 200
		want       []string
		wantErr    string // substring; empty means no error
	}{
		{
			name: "decodes and normalizes model list",
			body: `{"models":[{"model":"llama3:latest"},{"model":"mistral"}]}`,
			want: []string{"llama3:latest", "mistral:latest"},
		},
		{
			name:       "server error surfaces as error",
			statusCode: http.StatusServiceUnavailable,
			wantErr:    "connect to ollama",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.statusCode != 0 {
					w.WriteHeader(tt.statusCode)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3:latest", "", false)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			got, err := c.listModelNames(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("listModelNames() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("listModelNames() error = %v, want nil", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("listModelNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScoreSurfacesDoneReasonOnParseFailure(t *testing.T) {
	tests := []struct {
		name       string
		ndjsonBody string
		wantErr    string
	}{
		{
			name: "truncated response reports length",
			ndjsonBody: `{"message":{"role":"assistant","content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"ok\"}]"},"done":true,"done_reason":"length","eval_count":42}
`,
			wantErr: `done_reason="length", completion_tokens=42`,
		},
		{
			name: "natural stop with bad json reports stop",
			ndjsonBody: `{"message":{"role":"assistant","content":"not json"},"done":true,"done_reason":"stop","eval_count":7}
`,
			wantErr: `done_reason="stop", completion_tokens=7`,
		},
		{
			name: "stream closes without a done chunk",
			ndjsonBody: `{"message":{"role":"assistant","content":"not json"},"done":false}
`,
			wantErr: `done_reason="no done signal (stream closed early)", completion_tokens=0`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(tt.ndjsonBody))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3:latest", "", false)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			items := []feeds.Item{{ID: "a", Title: "A"}}
			_, err = c.Score(t.Context(), "profile", items)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Score() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
