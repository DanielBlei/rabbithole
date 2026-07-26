package rank

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/feeds"
)

func testItems() []feeds.Item {
	return []feeds.Item{
		{ID: "a", Title: "Scaling vLLM"},
		{ID: "b", Title: "Intro to Python"},
	}
}

func TestParseScores(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		items   []feeds.Item
		want    map[string]ItemScore // keyed by item ID
		wantErr []string             // substrings that must all appear in the error; nil means no error expected
	}{
		{
			name:  "plain JSON",
			raw:   `{"scores":[{"index":1,"score":9,"reason":"deep inference work"},{"index":2,"score":2,"reason":"beginner"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 9, Reason: "deep inference work"},
				"b": {ID: "b", Score: 2, Reason: "beginner"},
			},
		},
		{
			name:  "fenced code block with prose, score above range clamps to 10",
			raw:   "Sure! Here are the scores:\n```json\n{\"scores\":[{\"index\":1,\"score\":12,\"reason\":\"x\"}]}\n```\nHope that helps.",
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 10, Reason: "x"},
			},
		},
		{
			// gemma3 (and other small models) sometimes emit a fractional score
			// despite the prompt asking for an int 0-10.
			name:  "fractional score rounds to nearest int",
			raw:   `{"scores":[{"index":1,"score":9.5,"reason":"x"}]}`,
			items: testItems()[:1],
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 10, Reason: "x"},
			},
		},
		{
			name:  "out-of-range index is dropped, valid index kept",
			raw:   `{"scores":[{"index":99,"score":5,"reason":"x"},{"index":1,"score":7,"reason":"y"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 7, Reason: "y"},
			},
		},
		{
			// gemma3:4b quotes the score more than any other schema violation.
			name:  "quoted score parses as a number",
			raw:   `{"scores":[{"index":1,"score":"9","reason":"deep inference work"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 9, Reason: "deep inference work"},
			},
		},
		{
			// Observed key typo: the colon ends up inside the field name.
			name:  "field name with trailing colon still maps",
			raw:   `{"scores":[{"index":1,"score":8,"reason:":"solid engineering write-up"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 8, Reason: "solid engineering write-up"},
			},
		},
		{
			name:  "reason wrapped in a list is joined",
			raw:   `{"scores":[{"index":1,"score":7,"reason":["throughput","and latency"]}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 7, Reason: "throughput and latency"},
			},
		},
		{
			// Invented fields (observed: "weight", "factorLevel") are ignored
			// rather than failing the decode.
			name:  "unknown fields are ignored",
			raw:   `{"scores":[{"index":1,"score":6,"reason":"x","weight":8,"factorLevel":"low"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 6, Reason: "x"},
			},
		},
		{
			// The dominant truncation shape: generation stops inside the last
			// rationale. Earlier entries are intact and must survive.
			name:  "truncated mid-reason keeps completed entries and the cut one",
			raw:   `{"scores":[{"index":1,"score":9,"reason":"deep"},{"index":2,"score":3,"reason":"Provides basic context for LLMs but doesn`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 9, Reason: "deep"},
				"b": {ID: "b", Score: 3, Reason: "Provides basic context for LLMs but doesn"},
			},
		},
		{
			// Cut before the value: that entry can't be salvaged, but dropping
			// it must not cost the entries that completed.
			name:  "truncated mid-field drops only the incomplete entry",
			raw:   `{"scores":[{"index":1,"score":9,"reason":"deep"},{"index":2,"score":`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 9, Reason: "deep"},
			},
		},
		{
			// Cut after a lone backslash: appending the closing quote as-is
			// would escape it and leave the string open.
			name:  "truncated inside an escape sequence",
			raw:   `{"scores":[{"index":1,"score":9,"reason":"quoting \"vLLM` + `\`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 9, Reason: `quoting "vLLM`},
			},
		},
		{
			// A brace inside a rationale must not be mistaken for the object's end.
			name:  "brace inside a reason string does not end the object",
			raw:   `{"scores":[{"index":1,"score":4,"reason":"uses {json} mode"}]} trailing prose`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 4, Reason: "uses {json} mode"},
			},
		},
		{
			// The model ends the string with `”` instead of `"`, so its `}]}`
			// and everything after land inside the rationale.
			name:  "false close with a typographic quote is trimmed",
			raw:   `{"scores":[{"index":1,"score":6,"reason":"Clear and actionable guidance.”}]} ✅️👍 🌟🌟🌟"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 6, Reason: "Clear and actionable guidance."},
			},
		},
		{
			name:  "typographic quote inside prose is kept",
			raw:   `{"scores":[{"index":1,"score":7,"reason":"Explains the “why” behind KV cache reuse"}]}`,
			items: testItems(),
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 7, Reason: `Explains the “why” behind KV cache reuse`},
			},
		},
		{
			name:    "no JSON object in response",
			raw:     "no json here",
			items:   testItems(),
			wantErr: []string{"no json here"},
		},
		{
			// Shape collapse beyond repair (observed: {"scores=[[7,8,7]]}).
			// Still an error, but it must not panic or hang.
			name:    "unsalvageable shape errors",
			raw:     `{"scores=[[7,8,7]]}`,
			items:   testItems(),
			wantErr: []string{"parse scores json"},
		},
		{
			name:    "error names offending indices and valid range",
			raw:     `{"scores":[{"index":99,"score":9,"reason":"x"}]}`,
			items:   testItems(),
			wantErr: []string{"[99]", "want 1-2"},
		},
		{
			// qwen3:0.6b (and other weak models) sometimes hardcode index 0
			// regardless of batch size or the prompt's 1-based numbering. A
			// single-item batch has only one possible target, so any index
			// value should map to it.
			name:  "single-item batch accepts any index",
			raw:   `{"scores":[{"index":0,"score":9,"reason":"deep inference work"}]}`,
			items: testItems()[:1],
			want: map[string]ItemScore{
				"a": {ID: "a", Score: 9, Reason: "deep inference work"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScores(tt.raw, tt.items)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ParseScores() error = nil, want error containing %v", tt.wantErr)
				}
				for _, sub := range tt.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("ParseScores() error = %q, want it to contain %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseScores() unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d scores, want %d: %+v", len(got), len(tt.want), got)
			}
			for _, s := range got {
				want, ok := tt.want[s.ID]
				if !ok {
					t.Errorf("unexpected score for item %q: %+v", s.ID, s)
					continue
				}
				if s != want {
					t.Errorf("item %q = %+v, want %+v", s.ID, s, want)
				}
			}
		})
	}
}

func TestModelTuning(t *testing.T) {
	tests := []struct {
		name       string
		tuning     ModelTuning
		items      int
		think      bool
		wantBudget int
		wantReason int // schema maxLength
	}{
		{
			name: "zero value uses defaults", items: 5,
			wantBudget: 256 + 256*5, wantReason: 200,
		},
		{
			name: "think adds its allowance", items: 5, think: true,
			wantBudget: 256 + 256*5 + 2048, wantReason: 200,
		},
		{
			name: "max_tokens overrides the auto-size", items: 5, think: true,
			tuning:     ModelTuning{MaxTokens: 900},
			wantBudget: 900, wantReason: 200,
		},
		{
			name: "partial config keeps defaults for the rest", items: 2,
			tuning:     ModelTuning{TokensPerItem: 64, ReasonMaxChars: 80},
			wantBudget: 256 + 64*2, wantReason: 80,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tuning.Budget(tt.items, tt.think); got != tt.wantBudget {
				t.Errorf("Budget(%d, %v) = %d, want %d", tt.items, tt.think, got, tt.wantBudget)
			}
			schema := tt.tuning.Schema()
			if !json.Valid([]byte(schema)) {
				t.Fatalf("Schema() is not valid JSON: %s", schema)
			}
			if want := fmt.Sprintf(`"maxLength": %d`, tt.wantReason); !strings.Contains(schema, want) {
				t.Errorf("Schema() missing %q:\n%s", want, schema)
			}
		})
	}
}
