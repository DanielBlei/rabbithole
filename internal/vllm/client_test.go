package vllm

import (
	"net/http"
	"net/http/httptest"
	"slices"
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
		{name: "model present succeeds", body: `{"data":[{"id":"llama3"}]}`},
		{name: "model missing fails", body: `{"data":[{"id":"other"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3", "", false)
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

func TestListModelNames(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		statusCode int // 0 means 200
		want       []string
		wantErr    string // substring; empty means no error
	}{
		{
			name: "decodes model list",
			body: `{"data":[{"id":"llama3"},{"id":"mistral"}]}`,
			want: []string{"llama3", "mistral"},
		},
		{
			name:       "server error surfaces as error",
			statusCode: http.StatusServiceUnavailable,
			wantErr:    "vLLM /v1/models returned",
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

			c, err := New(srv.URL, "llama3", "", false)
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

func TestScoreSurfacesFinishReasonOnParseFailure(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "truncated response reports length",
			body:    `{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"ok\"}]"},"finish_reason":"length"}],"usage":{"completion_tokens":42}}`,
			wantErr: `finish_reason="length", completion_tokens=42`,
		},
		{
			name:    "natural stop with bad json reports stop",
			body:    `{"choices":[{"message":{"content":"not json"},"finish_reason":"stop"}],"usage":{"completion_tokens":7}}`,
			wantErr: `finish_reason="stop", completion_tokens=7`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3", "", false)
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
