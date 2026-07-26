package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

			c, err := New(srv.URL, "llama3:latest", "", false, rank.ModelTuning{})
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

			c, err := New(srv.URL, "llama3:latest", "", true, rank.ModelTuning{})
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

			c, err := New(srv.URL, "llama3:latest", "", false, rank.ModelTuning{})
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
			// Cut before any entry completed, so there is nothing to salvage.
			// A response truncated mid-rationale is repaired instead — see
			// TestScoreSalvagesTruncatedResponse.
			name: "truncation beyond repair reports length",
			ndjsonBody: `{"message":{"role":"assistant","content":"{\"scores\":[{\"index"},"done":true,"done_reason":"length","eval_count":42}
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

			c, err := New(srv.URL, "llama3:latest", "", false, rank.ModelTuning{})
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
	body := `{"message":{"role":"assistant","content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"Very relevant to LLM serving"},"done":true,"done_reason":"length","eval_count":42}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3:latest", "", false, rank.ModelTuning{})
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
// verdicts at the source: the schema Ollama compiles to a sampling grammar, and
// the token cap that bounds a runaway rationale.
func TestScoreRequestConstrainsOutput(t *testing.T) {
	var got struct {
		Format  json.RawMessage `json:"format"`
		Options struct {
			NumPredict int `json:"num_predict"`
		} `json:"options"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{\"scores\":[{\"index\":1,\"score\":9,\"reason\":\"x\"}]}"},"done":true,"done_reason":"stop","eval_count":9}
`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "llama3:latest", "", false, rank.ModelTuning{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	items := []feeds.Item{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}
	if _, err := c.Score(t.Context(), "profile", items); err != nil {
		t.Fatalf("Score() error = %v", err)
	}
	if !json.Valid(got.Format) || len(got.Format) == 0 || got.Format[0] != '{' {
		t.Errorf("format = %s, want the JSON schema object", got.Format)
	}
	if want := (rank.ModelTuning{}).Budget(len(items), false); got.Options.NumPredict != want {
		t.Errorf("num_predict = %d, want %d", got.Options.NumPredict, want)
	}
}
