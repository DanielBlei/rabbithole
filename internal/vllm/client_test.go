// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/rank"
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

			c, err := New(srv.URL, "llama3", "", false, rank.ModelTuning{})
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

			c, err := New(srv.URL, "llama3", "", false, rank.ModelTuning{})
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
			// Cut before any entry completed, so there is nothing to salvage.
			// A response truncated mid-rationale is repaired instead — see
			// TestScoreSalvagesTruncatedResponse.
			name:    "truncation beyond repair reports length",
			body:    `{"choices":[{"message":{"content":"{\"scores\":[{\"index"},"finish_reason":"length"}],"usage":{"completion_tokens":42}}`,
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

			c, err := New(srv.URL, "llama3", "", false, rank.ModelTuning{})
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

// TestScoreSalvagesTruncatedResponse covers the dominant real-world failure:
// the model runs long on the last rationale and generation stops mid-string.
// The scores that did arrive are good and must survive — dropping the batch
// cost whole articles their place in the digest.
func TestScoreSalvagesTruncatedResponse(t *testing.T) {
	body := `{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"Very relevant to LLM serving"},"finish_reason":"length"}],"usage":{"completion_tokens":42}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3", "", false, rank.ModelTuning{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := c.Score(t.Context(), "profile", []feeds.Item{{ID: "a", Title: "A"}})
	if err != nil {
		t.Fatalf("Score() error = %v, want the truncated response salvaged", err)
	}
	want := []rank.ItemScore{{ID: "a", Score: 9, Reason: "Very relevant to LLM serving"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Score() = %+v, want %+v", got, want)
	}
}

// TestScoreRequestConstrainsOutput pins the two settings that stop malformed
// verdicts at the source: the schema the server uses for guided decoding, and
// the token cap that bounds a runaway rationale.
func TestScoreRequestConstrainsOutput(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(
			[]byte(
				`{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"x\"}]}"},"finish_reason":"stop"}],"usage":{"completion_tokens":9}}`,
			),
		)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3", "", false, rank.ModelTuning{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	items := []feeds.Item{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}
	if _, err := c.Score(t.Context(), "profile", items); err != nil {
		t.Fatalf("Score() error = %v", err)
	}
	if got.ResponseFormat.Type != "json_schema" {
		t.Errorf("response_format.type = %q, want %q", got.ResponseFormat.Type, "json_schema")
	}
	if got.ResponseFormat.JSONSchema == nil || !json.Valid(got.ResponseFormat.JSONSchema.Schema) {
		t.Errorf("response_format.json_schema = %+v, want the scoring schema", got.ResponseFormat.JSONSchema)
	}
	if want := (rank.ModelTuning{}).Budget(len(items), false); got.MaxTokens != want {
		t.Errorf("max_tokens = %d, want %d", got.MaxTokens, want)
	}
}
