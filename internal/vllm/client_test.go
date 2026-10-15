// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"encoding/json"
	"io"
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
		name       string
		models     string
		probeCode  int // chat-probe response status; 0 means 200
		wantErr    bool
		wantErrTxt string
	}{
		{name: "model present succeeds", models: `{"data":[{"id":"llama3"}]}`},
		{name: "model missing accepted via chat probe", models: `{"data":[{"id":"other"}]}`},
		{
			name:       "model missing and probe rejected fails",
			models:     `{"data":[{"id":"other"}]}`,
			probeCode:  http.StatusNotFound,
			wantErr:    true,
			wantErrTxt: "chat probe",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case http.MethodGet + " /v1/models":
					_, _ = w.Write([]byte(tt.models))
				case http.MethodPost + " /v1/chat/completions":
					if tt.probeCode != 0 {
						w.WriteHeader(tt.probeCode)
						_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
						return
					}
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
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
			if tt.wantErrTxt != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErrTxt)) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErrTxt)
			}
		})
	}
}

func TestProbeRequestShape(t *testing.T) {
	withFastValidateRetry(t)

	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case http.MethodPost + " /v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &raw)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"other"}]}`))
		}
	}))
	defer srv.Close()

	c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() error = %v, want nil (probe should accept)", err)
	}
	if raw == nil {
		t.Fatal("no chat probe was sent")
	}
	if raw["model"] != "llama3" {
		t.Errorf("probe model = %v, want llama3", raw["model"])
	}
	if raw["max_tokens"] != float64(1) {
		t.Errorf("probe max_tokens = %v, want 1", raw["max_tokens"])
	}
	if raw["stream"] != false {
		t.Errorf("probe stream = %v, want false", raw["stream"])
	}
	if _, ok := raw["response_format"]; ok {
		t.Error("probe request carries response_format; the minimal probe must not")
	}
}

func TestProbeRetry(t *testing.T) {
	withFastValidateRetry(t)

	for _, tc := range []struct {
		name      string
		status    int
		wantCalls int
	}{
		{"not found fails fast", http.StatusNotFound, 1},
		{"unauthorized fails fast", http.StatusUnauthorized, 1},
		{"rate limited retries", http.StatusTooManyRequests, 3},
		{"server error retries", http.StatusServiceUnavailable, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if err := c.probeModelWithRetry(t.Context(), ""); err == nil {
				t.Fatal("probeModelWithRetry() error = nil, want non-nil")
			}
			if calls != tc.wantCalls {
				t.Fatalf("probe calls = %d, want %d", calls, tc.wantCalls)
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
			wantErr:    "vllm /v1/models returned",
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

			c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
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

			c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
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

	c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
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

	c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, "")
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

func TestScoreFlavorDialect(t *testing.T) {
	for _, tc := range []struct {
		name          string
		flavor        Flavor
		think         bool
		wantTokens    string // the token cap field that must be present
		absentTokens  string // and the one that must not
		wantEffort    any    // nil means the field is absent
		wantKwargsOff bool   // chat_template_kwargs.enable_thinking == false
	}{
		{"openai think on", OpenAI, true, "max_completion_tokens", "max_tokens", "medium", false},
		{"openai think off", OpenAI, false, "max_completion_tokens", "max_tokens", "low", false},
		{"vllm think on", VLLM, true, "max_tokens", "max_completion_tokens", nil, false},
		{"vllm think off", VLLM, false, "max_tokens", "max_completion_tokens", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(
					`{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"x\"}]}"},"finish_reason":"stop"}],"usage":{"completion_tokens":9}}`,
				))
			}))
			defer srv.Close()

			c, err := New(tc.flavor, srv.URL, "llama3", "", tc.think, "medium", rank.ModelTuning{}, "")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := c.Score(t.Context(), "profile", []feeds.Item{{ID: "a", Title: "A"}}); err != nil {
				t.Fatalf("Score() error = %v", err)
			}
			if _, ok := got[tc.wantTokens]; !ok {
				t.Errorf("%s missing from request", tc.wantTokens)
			}
			if _, ok := got[tc.absentTokens]; ok {
				t.Errorf("%s present, want absent", tc.absentTokens)
			}
			effort, ok := got["reasoning_effort"]
			if tc.wantEffort == nil && ok || tc.wantEffort != nil && effort != tc.wantEffort {
				t.Errorf("reasoning_effort = %v, want %v", effort, tc.wantEffort)
			}
			kwargs, _ := got["chat_template_kwargs"].(map[string]any)
			if off := kwargs["enable_thinking"] == false; off != tc.wantKwargsOff {
				t.Errorf("chat_template_kwargs = %v, want enable_thinking=false: %v", kwargs, tc.wantKwargsOff)
			}
		})
	}
}

func TestResolveEffort(t *testing.T) {
	withFastValidateRetry(t)

	for _, tc := range []struct {
		name     string
		think    bool
		effort   string
		accepts  func(effort string) int // probe status for a reasoning_effort ("" = field absent)
		want     string                  // reasoning_effort Score then sends; "" = absent
		wantErr  bool
		wantSeen []string
	}{
		{
			name:  "think off takes none when the model accepts it",
			think: false, effort: "medium",
			accepts:  func(string) int { return http.StatusOK },
			want:     "none",
			wantSeen: []string{"none"},
		},
		{
			name:  "think off walks down to the lowest accepted",
			think: false, effort: "medium",
			accepts:  only("low"),
			want:     "low",
			wantSeen: []string{"none", "minimal", "low"},
		},
		{
			name:  "think off omits the field when no effort is accepted",
			think: false, effort: "medium",
			accepts:  only(""),
			want:     "",
			wantSeen: []string{"none", "minimal", "low", ""},
		},
		{
			name:  "think on keeps the configured effort",
			think: true, effort: "high",
			accepts:  func(string) int { return http.StatusOK },
			want:     "high",
			wantSeen: []string{"high"},
		},
		{
			name:  "think on omits an effort the model rejects",
			think: true, effort: "xhigh",
			accepts:  only(""),
			want:     "",
			wantSeen: []string{"xhigh", ""},
		},
		{
			name:  "auth failure fails validation",
			think: false, effort: "medium",
			accepts:  func(string) int { return http.StatusUnauthorized },
			wantErr:  true,
			wantSeen: []string{"none"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			var scored map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt"}]}`))
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if _, scoring := body["response_format"]; scoring {
					scored = body
					_, _ = w.Write([]byte(
						`{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"x\"}]}"},"finish_reason":"stop"}]}`,
					))
					return
				}
				effort, _ := body["reasoning_effort"].(string)
				seen = append(seen, effort)
				w.WriteHeader(tc.accepts(effort))
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			c, err := New(OpenAI, srv.URL, "gpt", "", tc.think, tc.effort, rank.ModelTuning{}, "")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = c.Validate(t.Context())
			if !slices.Equal(seen, tc.wantSeen) {
				t.Errorf("probed efforts = %q, want %q", seen, tc.wantSeen)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("Validate() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if _, err := c.Score(t.Context(), "profile", []feeds.Item{{ID: "a", Title: "A"}}); err != nil {
				t.Fatalf("Score() error = %v", err)
			}
			got, ok := scored["reasoning_effort"]
			if tc.want == "" && ok || tc.want != "" && got != tc.want {
				t.Errorf("Score reasoning_effort = %v, want %q", got, tc.want)
			}
		})
	}
}

// only accepts exactly the given reasoning_effort and rejects the rest with a 400.
func only(accepted string) func(string) int {
	return func(effort string) int {
		if effort == accepted {
			return http.StatusOK
		}
		return http.StatusBadRequest
	}
}

func TestErrorsNameTheFlavor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c, err := New(OpenAI, srv.URL, "gpt", "", true, "medium", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = c.Score(t.Context(), "profile", []feeds.Item{{ID: "a", Title: "A"}})
	if err == nil || !strings.HasPrefix(err.Error(), "openai chat returned 400") {
		t.Fatalf("Score() error = %v, want it to name the openai provider", err)
	}
}

// TestScoreOmitsSystemMessageWhenPromptEmpty pins the behavior a disabled system_prompt
// depends on: an empty systemPrompt must drop the message entirely, not send it empty, since
// only an absent system message lets a served model's own default chat-template prompt apply.
func TestScoreOmitsSystemMessageWhenPromptEmpty(t *testing.T) {
	for _, tc := range []struct {
		name         string
		systemPrompt string
		wantRoles    []string
	}{
		{"empty prompt sends no system message", "", []string{"user"}},
		{"set prompt sends it first", "be nice", []string{"system", "user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got chatRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(
					`{"choices":[{"message":{"content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"x\"}]}"},"finish_reason":"stop"}],"usage":{"completion_tokens":9}}`,
				))
			}))
			defer srv.Close()

			c, err := New(VLLM, srv.URL, "llama3", "", false, "medium", rank.ModelTuning{}, tc.systemPrompt)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := c.Score(t.Context(), "profile", []feeds.Item{{ID: "a", Title: "A"}}); err != nil {
				t.Fatalf("Score() error = %v", err)
			}
			var roles []string
			for _, m := range got.Messages {
				roles = append(roles, m.Role)
			}
			if !slices.Equal(roles, tc.wantRoles) {
				t.Fatalf("message roles = %v, want %v", roles, tc.wantRoles)
			}
			if tc.systemPrompt != "" && got.Messages[0].Content != tc.systemPrompt {
				t.Errorf("system message content = %q, want %q", got.Messages[0].Content, tc.systemPrompt)
			}
		})
	}
}
