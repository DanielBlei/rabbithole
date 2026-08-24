// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/eval"
)

// resetBenchmarkFlags puts the package-level flag vars back to the defaults
// cobra would have installed, so each case starts from a bare invocation.
func resetBenchmarkFlags() {
	benchRepeats = eval.DefaultRepeats
	benchLimit = 0
	benchProvider, benchHost, benchModel = "", "", ""
	benchNoThink, benchShowWhy = false, false
	benchFormat, benchOutput = string(eval.FormatText), ""
}

func resetAuditFlags() {
	auditRatedOnly, auditAll, auditNewest = false, false, false
	auditLimit = eval.DefaultLimit
	auditSeed = 0
	auditSince, auditSource, auditScoredBy = "", "", ""
	auditFormat, auditOutput = string(eval.FormatText), ""
}

func TestResolveBenchmarkOptions(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		limitSet bool
		setup    func()
		wantErr  bool
		check    func(t *testing.T, o eval.BenchmarkOptions)
	}{
		{
			name: "bare benchmark defaults to the golden file beside the config",
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if o.Path != defaultBenchmarkPath {
					t.Errorf("Path = %q, want %q", o.Path, defaultBenchmarkPath)
				}
				if o.Repeats != eval.DefaultRepeats {
					t.Errorf("Repeats = %d, want %d", o.Repeats, eval.DefaultRepeats)
				}
				if o.Output.Format != eval.FormatText {
					t.Errorf("Format = %q, want text", o.Output.Format)
				}
				if o.Limit != 0 {
					t.Errorf("Limit = %d, want 0 (every sample)", o.Limit)
				}
				if o.ShowWhy {
					t.Error("ShowWhy = true, want false by default")
				}
				if o.Output.Path != "" {
					t.Errorf("Output.Path = %q, want empty (stdout)", o.Output.Path)
				}
			},
		},
		{
			name: "positional argument overrides the default golden file",
			args: []string{"testdata/other.yaml"},
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if o.Path != "testdata/other.yaml" {
					t.Errorf("Path = %q, want testdata/other.yaml", o.Path)
				}
			},
		},
		{
			name:     "limit narrows the run",
			limitSet: true,
			setup:    func() { benchLimit = 5 },
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if o.Limit != 5 {
					t.Errorf("Limit = %d, want 5", o.Limit)
				}
			},
		},
		{
			name:     "limit below one is refused",
			limitSet: true,
			setup:    func() { benchLimit = 0 },
			wantErr:  true,
		},
		{
			name:  "show-why carries through",
			setup: func() { benchShowWhy = true },
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if !o.ShowWhy {
					t.Error("ShowWhy = false, want true")
				}
			},
		},
		{
			name:  "markdown format is accepted",
			setup: func() { benchFormat = "markdown" },
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if o.Output.Format != eval.FormatMarkdown {
					t.Errorf("Format = %q, want markdown", o.Output.Format)
				}
			},
		},
		{
			name:  "backend overrides carry through",
			setup: func() { benchProvider, benchHost, benchModel = "vllm", "http://x:8000", "gemma4:26b" },
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if o.Provider != "vllm" || o.Host != "http://x:8000" || o.Model != "gemma4:26b" {
					t.Errorf("overrides = %q/%q/%q, want vllm/http://x:8000/gemma4:26b", o.Provider, o.Host, o.Model)
				}
			},
		},
		{
			name:  "json format is accepted",
			setup: func() { benchFormat = "json" },
			check: func(t *testing.T, o eval.BenchmarkOptions) {
				if o.Output.Format != eval.FormatJSON {
					t.Errorf("Format = %q, want json", o.Output.Format)
				}
			},
		},
		{
			name:    "unknown format is refused",
			setup:   func() { benchFormat = "yaml" },
			wantErr: true,
		},
		{
			name:    "repeats below one is refused",
			setup:   func() { benchRepeats = 0 },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetBenchmarkFlags()
			if tt.setup != nil {
				tt.setup()
			}
			o, err := resolveBenchmarkOptions(tt.args, tt.limitSet)
			if tt.wantErr {
				if err == nil {
					t.Fatal("resolveBenchmarkOptions() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBenchmarkOptions() error = %v", err)
			}
			tt.check(t, o)
		})
	}
}

func TestResolveAuditOptions(t *testing.T) {
	// resolveAuditOptions takes now, so the bound it derives is exact rather
	// than approximate.
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		setup    func()
		limitSet bool
		wantErr  bool
		check    func(t *testing.T, o eval.AuditOptions)
	}{
		{
			name: "bare audit samples the default limit, unbounded window",
			check: func(t *testing.T, o eval.AuditOptions) {
				if o.Limit != eval.DefaultLimit {
					t.Errorf("Limit = %d, want %d", o.Limit, eval.DefaultLimit)
				}
				if !o.After.IsZero() {
					t.Errorf("After = %s, want zero (unbounded)", o.After)
				}
				if o.All || o.Newest || o.RatedOnly {
					t.Errorf("All/Newest/RatedOnly = %v/%v/%v, want all false", o.All, o.Newest, o.RatedOnly)
				}
			},
		},
		{
			name:  "--since resolves to an absolute bound",
			setup: func() { auditSince = "30d" },
			check: func(t *testing.T, o eval.AuditOptions) {
				want := now.Add(-30 * 24 * time.Hour)
				if !o.After.Equal(want) {
					t.Errorf("After = %s, want %s", o.After, want)
				}
			},
		},
		{
			name:  "--rated-only and the narrowing flags carry through",
			setup: func() { auditRatedOnly, auditSource, auditScoredBy = true, "Source One", "qwen3:0.6b" },
			check: func(t *testing.T, o eval.AuditOptions) {
				if !o.RatedOnly || o.Source != "Source One" || o.ScoredBy != "qwen3:0.6b" {
					t.Errorf("got %v/%q/%q, want true/Source One/qwen3:0.6b", o.RatedOnly, o.Source, o.ScoredBy)
				}
			},
		},
		{
			name:  "--all is allowed while --limit sits at its default",
			setup: func() { auditAll = true },
			check: func(t *testing.T, o eval.AuditOptions) {
				if !o.All {
					t.Error("All = false, want true")
				}
			},
		},
		{
			name:     "--all with an explicit --limit is refused",
			setup:    func() { auditAll, auditLimit = true, 5 },
			limitSet: true,
			wantErr:  true,
		},
		{
			name:    "--limit below one is refused",
			setup:   func() { auditLimit = 0 },
			wantErr: true,
		},
		{
			name:  "--seed alone is a reproducible draw",
			setup: func() { auditSeed = 42 },
			check: func(t *testing.T, o eval.AuditOptions) {
				if o.Seed != 42 {
					t.Errorf("Seed = %d, want 42", o.Seed)
				}
			},
		},
		{
			name:    "--seed with --newest is refused as a contradiction",
			setup:   func() { auditSeed, auditNewest = 42, true },
			wantErr: true,
		},
		{
			name:    "invalid --since",
			setup:   func() { auditSince = "nope" },
			wantErr: true,
		},
		{
			name:    "unknown format is refused",
			setup:   func() { auditFormat = "csv" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetAuditFlags()
			if tt.setup != nil {
				tt.setup()
			}
			o, err := resolveAuditOptions(now, tt.limitSet)
			if tt.wantErr {
				if err == nil {
					t.Fatal("resolveAuditOptions() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAuditOptions() error = %v", err)
			}
			tt.check(t, o)
		})
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    eval.Format
		wantErr bool
	}{
		{in: "text", want: eval.FormatText},
		{in: "markdown", want: eval.FormatMarkdown},
		{in: "json", want: eval.FormatJSON},
		{in: "", wantErr: true},
		{in: "MARKDOWN", wantErr: true},
		{in: "yaml", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := eval.ParseFormat(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseFormat(%q) error = nil, want non-nil", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFormat(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
