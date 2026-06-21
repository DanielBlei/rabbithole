package cmd

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DanielBlei/ai-searcher/internal/store"
)

func TestResolveListFilter(t *testing.T) {
	const defaultSince = 72 * time.Hour
	// f.After/f.Before are computed from time.Now() inside resolveListFilter,
	// captured a hair after this. ageOf reports how long ago t is relative to
	// a fresh now; tests assert that's within a minute of the expected window.
	ageOf := func(ts time.Time) time.Duration { return time.Since(ts) }
	approx := func(got, want time.Duration) bool { return got-want < time.Minute && want-got < time.Minute }

	tests := []struct {
		name   string
		since  string
		before string
		want   bool // wantErr
		check  func(t *testing.T, f store.ListFilter)
	}{
		{
			name: "bare list defaults After to the configured window",
			check: func(t *testing.T, f store.ListFilter) {
				if !approx(ageOf(f.After), defaultSince) {
					t.Errorf("After is %s ago, want ~%s", ageOf(f.After), defaultSince)
				}
				if !f.Before.IsZero() {
					t.Errorf("Before = %s, want zero (unbounded above)", f.Before)
				}
			},
		},
		{
			name:  "--since overrides the default window",
			since: "12h",
			check: func(t *testing.T, f store.ListFilter) {
				if !approx(ageOf(f.After), 12*time.Hour) {
					t.Errorf("After is %s ago, want ~12h", ageOf(f.After))
				}
			},
		},
		{
			name:   "--before pages older without re-imposing the default",
			before: "1d",
			check: func(t *testing.T, f store.ListFilter) {
				if !f.After.IsZero() {
					t.Errorf("After = %s, want zero (unbounded below)", f.After)
				}
				if !approx(ageOf(f.Before), 24*time.Hour) {
					t.Errorf("Before is %s ago, want ~1d", ageOf(f.Before))
				}
			},
		},
		{name: "invalid --since", since: "nope", want: true},
		{name: "invalid --before", before: "nope", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listStatus, listSource, listSort, listLimit = "", "", "", 0
			listSince, listBefore = tt.since, tt.before

			f, err := resolveListFilter(defaultSince)
			if tt.want {
				if err == nil {
					t.Fatal("resolveListFilter() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveListFilter() error = %v", err)
			}
			tt.check(t, f)
		})
	}
}

func TestApplyToEach(t *testing.T) {
	tests := []struct {
		name        string
		identifiers []string
		failOn      map[string]bool
		wantErr     bool
		wantCalls   []string
	}{
		{
			name:        "all succeed",
			identifiers: []string{"a", "b", "c"},
			failOn:      map[string]bool{},
			wantCalls:   []string{"a", "b", "c"},
		},
		{
			name:        "one fails, the rest still run",
			identifiers: []string{"a", "b", "c"},
			failOn:      map[string]bool{"b": true},
			wantErr:     true,
			wantCalls:   []string{"a", "b", "c"},
		},
		{
			name:        "all fail",
			identifiers: []string{"a", "b"},
			failOn:      map[string]bool{"a": true, "b": true},
			wantErr:     true,
			wantCalls:   []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := applyToEach(tt.identifiers, func(id string) error {
				calls = append(calls, id)
				if tt.failOn[id] {
					return errors.New("boom")
				}
				return nil
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("applyToEach() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}
