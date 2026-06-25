package rank

import (
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
			name:    "no JSON object in response",
			raw:     "no json here",
			items:   testItems(),
			wantErr: []string{"no json here"},
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
