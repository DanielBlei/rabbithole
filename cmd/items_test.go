package cmd

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/store"
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

func TestResolvePruneFilter(t *testing.T) {
	// resolvePruneFilter takes now, so the bounds it derives are exact rather
	// than approximate the way resolveListFilter's are.
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		all          bool
		source       string
		since        string
		before       string
		includeSaved bool
		wantErr      bool
		want         store.PruneFilter
	}{
		{
			name:    "no flags is refused rather than defaulted",
			wantErr: true,
		},
		{
			name:    "--include-saved alone is not a selection",
			wantErr: true,
		},
		{
			name: "--all empties the feed on purpose",
			all:  true,
			want: store.PruneFilter{All: true},
		},
		{
			name:         "--all with --include-saved leaves nothing",
			all:          true,
			includeSaved: true,
			want:         store.PruneFilter{All: true, IncludeSaved: true},
		},
		{
			name:    "--all contradicts --source",
			all:     true,
			source:  "S1",
			wantErr: true,
		},
		{
			name:    "--all contradicts --before",
			all:     true,
			before:  "30d",
			wantErr: true,
		},
		{
			name:   "--source leaves both bounds open",
			source: "Red Hat Emerging Tech",
			want:   store.PruneFilter{Source: "Red Hat Emerging Tech"},
		},
		{
			name:   "--before becomes an absolute upper bound",
			before: "30d",
			want:   store.PruneFilter{Before: now.Add(-30 * 24 * time.Hour)},
		},
		{
			name:  "--since becomes an absolute lower bound",
			since: "2d",
			want:  store.PruneFilter{After: now.Add(-2 * 24 * time.Hour)},
		},
		{
			name:   "--since and --before bound a window",
			since:  "30d",
			before: "7d",
			want: store.PruneFilter{
				After:  now.Add(-30 * 24 * time.Hour),
				Before: now.Add(-7 * 24 * time.Hour),
			},
		},
		{
			name:         "--include-saved rides along with a selection",
			source:       "S1",
			includeSaved: true,
			want:         store.PruneFilter{Source: "S1", IncludeSaved: true},
		},
		{name: "invalid --since", since: "nope", wantErr: true},
		{name: "invalid --before", before: "nope", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pruneAll = tt.all
			pruneSource, pruneSince, pruneBefore = tt.source, tt.since, tt.before
			pruneIncludeSaved, pruneDryRun = tt.includeSaved, false

			got, err := resolvePruneFilter(now)
			if tt.wantErr {
				if err == nil {
					t.Fatal("resolvePruneFilter() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePruneFilter() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("resolvePruneFilter() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReingestNote(t *testing.T) {
	const window = 14 * 24 * time.Hour
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		filter   store.PruneFilter
		wantNote bool
	}{
		{
			name:     "no upper bound reaches into the window",
			filter:   store.PruneFilter{Source: "S1"},
			wantNote: true,
		},
		{
			name:     "cutoff inside the window",
			filter:   store.PruneFilter{Before: now.Add(-7 * 24 * time.Hour)},
			wantNote: true,
		},
		{
			name:   "cutoff older than the window",
			filter: store.PruneFilter{Before: now.Add(-30 * 24 * time.Hour)},
		},
		{
			name:   "cutoff exactly at the window edge",
			filter: store.PruneFilter{Before: now.Add(-window)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reingestNote(tt.filter, now, window); (got != "") != tt.wantNote {
				t.Errorf("reingestNote() = %q, want note %v", got, tt.wantNote)
			}
		})
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: 14 * 24 * time.Hour, want: "14d"},
		{d: 24 * time.Hour, want: "1d"},
		{d: 36 * time.Hour, want: "36h0m0s"},
		{d: 12 * time.Hour, want: "12h0m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := humanDuration(tt.d); got != tt.want {
				t.Errorf("humanDuration(%s) = %q, want %q", tt.d, got, tt.want)
			}
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
