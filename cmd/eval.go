// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/claude"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/eval"
	"github.com/DanielBlei/rabbithole/internal/inference"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

// defaultBenchmarkPath sits beside the other configs, matching how the config
// file itself is defaulted.
const defaultBenchmarkPath = "./configs/golden.yaml"

// heuristicProvider is the model-free scorer, useful for exercising a benchmark
// run without a backend.
const heuristicProvider = "heuristic"

// claudeProvider is the eval-only reference scorer. It is deliberately absent
// from config's provider whitelist and from inference.Resolve, so it is
// reachable only through --provider on this command, never from ingest.
const claudeProvider = "claude"

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Measure how well scoring matches your interest profile",
	Long: "Two ways to check the scoring. `benchmark` re-scores a hand-authored fixture " +
		"with your current profile and system prompt, so you can tell whether an edit " +
		"helped. `audit` reads the scores already in the store and reports what happened. " +
		"Both are read-only and only produce a report.",
}

var (
	benchRepeats  int
	benchLimit    int
	benchProvider string
	benchHost     string
	benchModel    string
	benchBatch    int
	benchParallel int
	benchNoThink  bool
	benchShowWhy  bool
	benchFormat   string
	benchOutput   string
)

var (
	auditRatedOnly bool
	auditLimit     int
	auditAll       bool
	auditSeed      int64
	auditNewest    bool
	auditSince     string
	auditSource    string
	auditScoredBy  string
	auditFormat    string
	auditOutput    string
)

func init() {
	benchmarkCmd := &cobra.Command{
		Use:   "benchmark [golden-file]",
		Short: "Score a labelled golden dataset with the current profile and prompt, and report the gap",
		Long: "Loads a golden dataset — articles you scored by hand — re-scores every sample with " +
			"your current profile.md and system prompt, and reports how far the model landed from " +
			"your scores. Because it re-scores rather than reading stored values, two runs either " +
			"side of a profile edit are directly comparable.\n\n" +
			"Defaults to " + defaultBenchmarkPath + ". Copy configs/golden.example.yaml to start " +
			"one of your own. Nothing is written to the store.",
		Example: "  rabbithole eval benchmark\n" +
			"  rabbithole eval benchmark configs/golden.example.yaml\n" +
			"  rabbithole eval benchmark --model qwen3.6:35b --repeats 5\n" +
			"  rabbithole eval benchmark --format json --output-path before.json",
		Args: cobra.MaximumNArgs(1),
		RunE: runBenchmark,
	}
	benchmarkCmd.Flags().
		IntVar(&benchRepeats, "repeats", eval.DefaultRepeats, "score the dataset this many times and report the spread; the model has no temperature or seed, so a single run cannot tell a real change from jitter")
	benchmarkCmd.Flags().
		IntVar(&benchLimit, "limit", 0, "score only the first N samples, in file order; a quick pass while iterating, not a representative sample (default: every sample)")
	benchmarkCmd.Flags().
		StringVar(&benchProvider, "provider", "", "override the configured provider (ollama|vllm|heuristic)")
	benchmarkCmd.Flags().StringVar(&benchHost, "host", "", "override the configured backend host")
	benchmarkCmd.Flags().
		StringVar(&benchModel, "model", "", "override the configured model, e.g. to benchmark a bigger one than you ingest with")
	benchmarkCmd.Flags().
		IntVar(&benchBatch, "batch-size", 0, "override the configured articles-per-request; changes what is measured, so use the same value on every run you compare (default: your config's)")
	benchmarkCmd.Flags().
		IntVar(&benchParallel, "max-parallel", 0, "override the configured requests in flight; for claude this also lifts the forced serial run (default: your config's, or 1 for claude)")
	benchmarkCmd.Flags().
		BoolVar(&benchNoThink, "no-think", false, "override config to disable model reasoning/thinking for this run")
	benchmarkCmd.Flags().
		BoolVar(&benchShowWhy, "show-why", false, "also print the model's stated reason and your golden note beside each sample")
	benchmarkCmd.Flags().
		StringVar(&benchFormat, "format", string(eval.FormatText), "report format (text|markdown|json)")
	benchmarkCmd.Flags().StringVar(&benchOutput, "output-path", "", "write the report here (default: stdout)")

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Report on the scores already in the store, and what your ratings say about them",
		Long: "Reads recorded scores and your own ratings, and reports which sources earn their " +
			"slot, where the model disagreed with your thumbs, and how scores are distributed. " +
			"Reads historical values rather than re-scoring, so it describes what happened " +
			"rather than testing a change. No model is contacted and nothing is written.",
		Args: cobra.NoArgs,
		// Hidden until the report is built. The flags are wired and validated,
		// but a subcommand that only ever exits non-zero is worse than one that
		// is not offered yet.
		Hidden: true,
		RunE:   runAudit,
	}
	auditCmd.Flags().
		BoolVar(&auditRatedOnly, "rated-only", false, "only items you rated, so the report is built from your own verdicts (an empty note is fine)")
	auditCmd.Flags().IntVar(&auditLimit, "limit", eval.DefaultLimit, "max items to sample")
	auditCmd.Flags().BoolVar(&auditAll, "all", false, "use every matching item; cannot be combined with --limit")
	auditCmd.Flags().
		Int64Var(&auditSeed, "seed", 0, "seed the random sample so a run can be repeated (default: a fresh draw each run)")
	auditCmd.Flags().
		BoolVar(&auditNewest, "newest", false, "take the most recent items instead of a random sample; biased toward whichever feeds ran last")
	auditCmd.Flags().
		StringVar(&auditSince, "since", "", "only items recorded within this long ago, e.g. 30d, 12h (default: unbounded)")
	auditCmd.Flags().StringVar(&auditSource, "source", "", "only items from this source")
	auditCmd.Flags().
		StringVar(&auditScoredBy, "scored-by", "", "only items scored by this model; llm_score_model is the only provenance stored, so a mixed sample can compare rows scored under different configs")
	auditCmd.Flags().StringVar(&auditFormat, "format", string(eval.FormatText), "report format (text|markdown|json)")
	auditCmd.Flags().StringVar(&auditOutput, "output-path", "", "write the report here (default: stdout)")

	evalCmd.AddCommand(benchmarkCmd, auditCmd)
	rootCmd.AddCommand(evalCmd)
}

// resolveOutput turns the --format/--output-path pair into an eval.Output.
func resolveOutput(format, path string) (eval.Output, error) {
	f, err := eval.ParseFormat(format)
	if err != nil {
		return eval.Output{}, fmt.Errorf("--format: %w", err)
	}
	return eval.Output{Format: f, Path: path}, nil
}

// resolveBenchmarkOptions turns the `eval benchmark` flags and its optional
// path argument into eval.BenchmarkOptions. Think is left to the caller, which
// needs the config to know what it is overriding.
func resolveBenchmarkOptions(args []string, limitSet bool) (eval.BenchmarkOptions, error) {
	opts := eval.BenchmarkOptions{
		Path:        defaultBenchmarkPath,
		Repeats:     benchRepeats,
		Limit:       benchLimit,
		Provider:    benchProvider,
		Host:        benchHost,
		Model:       benchModel,
		BatchSize:   benchBatch,
		MaxParallel: benchParallel,
		ShowWhy:     benchShowWhy,
	}
	if len(args) == 1 {
		opts.Path = args[0]
	}
	if opts.Repeats < 1 {
		return eval.BenchmarkOptions{}, fmt.Errorf("--repeats must be at least 1, got %d", opts.Repeats)
	}
	if limitSet && opts.Limit < 1 {
		return eval.BenchmarkOptions{}, fmt.Errorf("--limit must be at least 1, got %d", opts.Limit)
	}
	if opts.BatchSize < 0 {
		return eval.BenchmarkOptions{}, fmt.Errorf("--batch-size must be at least 1, got %d", opts.BatchSize)
	}
	if opts.MaxParallel < 0 {
		return eval.BenchmarkOptions{}, fmt.Errorf("--max-parallel must be at least 1, got %d", opts.MaxParallel)
	}
	out, err := resolveOutput(benchFormat, benchOutput)
	if err != nil {
		return eval.BenchmarkOptions{}, err
	}
	opts.Output = out
	return opts, nil
}

// resolveAuditOptions turns the `eval audit` flags into eval.AuditOptions,
// converting --since (a duration back from now) into the absolute bound the
// query wants. limitSet reports whether --limit was passed, which is what
// separates an explicit narrowing from the default when --all is also given.
func resolveAuditOptions(now time.Time, limitSet bool) (eval.AuditOptions, error) {
	opts := eval.AuditOptions{
		RatedOnly: auditRatedOnly,
		Source:    auditSource,
		ScoredBy:  auditScoredBy,
		Limit:     auditLimit,
		All:       auditAll,
		Seed:      auditSeed,
		Newest:    auditNewest,
	}
	if auditSince != "" {
		d, err := config.ParseDuration(auditSince)
		if err != nil {
			return eval.AuditOptions{}, fmt.Errorf("--since: %w", err)
		}
		opts.After = now.Add(-d)
	}
	if opts.All && limitSet {
		return eval.AuditOptions{}, errors.New("--all cannot be combined with --limit")
	}
	if !opts.All && opts.Limit < 1 {
		return eval.AuditOptions{}, fmt.Errorf("--limit must be at least 1, got %d", opts.Limit)
	}
	// A seeded draw and a recency order are different sampling strategies, and
	// honouring both would silently ignore one.
	if opts.Newest && opts.Seed != 0 {
		return eval.AuditOptions{}, errors.New(
			"--seed cannot be combined with --newest; --newest is already deterministic",
		)
	}
	out, err := resolveOutput(auditFormat, auditOutput)
	if err != nil {
		return eval.AuditOptions{}, err
	}
	opts.Output = out
	return opts, nil
}

func runBenchmark(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	opts, err := resolveBenchmarkOptions(args, cmd.Flags().Changed("limit"))
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	applyBackendOverride(cfg, opts)

	// think comes from config; --no-think overrides it for one-off runs, the
	// same precedence ingest uses.
	opts.Think = *cfg.Inference.Think
	if cmd.Flags().Changed("no-think") {
		opts.Think = false
	}

	profile, err := cfg.LoadProfile()
	if err != nil {
		return err
	}
	ds, err := eval.Load(opts.Path)
	if err != nil {
		return missingGoldenError(opts.Path, err)
	}
	datasetSamples := len(ds.Samples)
	ds = ds.Limit(opts.Limit)

	log.Info().
		Str("fixture", opts.Path).
		Int("samples", len(ds.Samples)).
		Int("of", datasetSamples).
		Str("provider", cfg.Inference.Provider).
		Str("model", cfg.Inference.Model).
		Bool("think", opts.Think).
		Msg("benchmark loaded")
	log.Debug().
		Int("profile_chars", len(profile)).
		Str("profile", cfg.Profile).
		Str("benchmark_hash", ds.Hash).
		Msg("benchmark inputs")

	systemPrompt, err := cfg.Inference.LoadSystemPrompt()
	if err != nil {
		return err
	}
	scorer, err := resolveJudgeScorer(ctx, cfg.Inference, opts.Think, systemPrompt)
	if err != nil {
		return err
	}

	// The heuristic scorer takes no model, so naming one in the report would
	// credit a run to a model that never saw it.
	model := cfg.Inference.Model
	if cfg.Inference.Provider == heuristicProvider {
		model = ""
	}

	res, err := eval.Run(ctx, eval.RunConfig{
		Dataset:        ds,
		Profile:        profile,
		Prompt:         systemPrompt,
		Scorer:         scorer,
		BatchSize:      cfg.Inference.BatchSize,
		MaxParallel:    cfg.Inference.MaxParallel,
		Repeats:        opts.Repeats,
		DatasetSamples: datasetSamples,
		Limit:          opts.Limit,
		Provider:       cfg.Inference.Provider,
		Model:          model,
		Think:          opts.Think,
	})
	if err != nil {
		return err
	}
	// An alias like "sonnet" resolves to a dated model. Record what ran, so two
	// reports months apart are not both labelled "sonnet".
	if rm, ok := scorer.(interface{ ResolvedModel() string }); ok {
		if m := rm.ResolvedModel(); m != "" {
			res.Info.Model = m
		}
	}
	rep := eval.Summarize(res, ds)
	return writeReport(rep, opts.Output, eval.RenderOptions{
		Format:  opts.Output.Format,
		ShowWhy: opts.ShowWhy,
	})
}

// missingGoldenError points at the example dataset when the default path is the
// one that is missing, since a fresh checkout does not ship configs/golden.yaml.
func missingGoldenError(path string, err error) error {
	if path != defaultBenchmarkPath || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("no golden dataset at %s; copy configs/golden.example.yaml to %s, "+
		"or pass a path", defaultBenchmarkPath, defaultBenchmarkPath)
}

// writeReport renders the report to the requested destination, stdout when no
// path was given. Opening the file is this layer's job; the renderers only
// ever see a writer.
func writeReport(rep *eval.Report, out eval.Output, render eval.RenderOptions) error {
	w := io.Writer(os.Stdout)
	if out.Path != "" {
		f, err := os.Create(out.Path)
		if err != nil {
			return err
		}
		defer func() {
			if err := f.Close(); err != nil {
				log.Warn().Err(err).Str("path", out.Path).Msg("report close failed")
			}
		}()
		w = f
	}

	if err := rep.Write(w, render); err != nil {
		return err
	}
	if out.Path != "" {
		fmt.Printf("Wrote %s\n", out.Path)
	}
	return nil
}

func runAudit(cmd *cobra.Command, _ []string) error {
	opts, err := resolveAuditOptions(time.Now(), cmd.Flags().Changed("limit"))
	if err != nil {
		return err
	}
	// Loaded to fail early on a bad config, and because the report names the
	// profile it was judged against.
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log.Debug().
		Bool("rated_only", opts.RatedOnly).
		Int("limit", opts.Limit).
		Bool("all", opts.All).
		Int64("seed", opts.Seed).
		Bool("newest", opts.Newest).
		Str("source", opts.Source).
		Str("scored_by", opts.ScoredBy).
		Str("db", cfg.Store.DBPath).
		Str("format", string(opts.Output.Format)).
		Msg("audit options resolved")

	return errNotImplemented("eval audit")
}

// applyBackendOverride folds the --provider/--host/--model flags onto the
// loaded config. Empty flags leave the configured value alone.
func applyBackendOverride(cfg *config.Config, opts eval.BenchmarkOptions) {
	if opts.Provider != "" {
		cfg.Inference.Provider = opts.Provider
	}
	if opts.Host != "" {
		cfg.Inference.Host = opts.Host
	}
	if opts.Model != "" {
		cfg.Inference.Model = opts.Model
	}
	if opts.BatchSize > 0 {
		cfg.Inference.BatchSize = opts.BatchSize
	}
	// One `claude -p` is a whole Claude Code process against a single shared
	// allowance, so the benchmark serialises them unless asked otherwise.
	switch {
	case opts.MaxParallel > 0:
		cfg.Inference.MaxParallel = opts.MaxParallel
	case cfg.Inference.Provider == claudeProvider:
		cfg.Inference.MaxParallel = 1
	}
}

// resolveJudgeScorer adds the eval-only Claude backend to the live set. It
// lives here rather than in internal/inference so the ingest path, which
// imports that package, has no route to it.
func resolveJudgeScorer(
	ctx context.Context, cfg config.InferenceConfig, think bool, systemPrompt string,
) (rank.Scorer, error) {
	if cfg.Provider != claudeProvider {
		return inference.Resolve(ctx, cfg, think, systemPrompt)
	}
	// --system-prompt replaces Claude Code's own prompt. With none, its agent
	// persona would score the fixture and the report would not be comparable.
	// Refused rather than warned: the run costs subscription allowance.
	if systemPrompt == "" {
		return nil, errors.New("system_prompt: false cannot be used with --provider claude; " +
			"without a scoring prompt Claude scores with its own agent persona and the run is " +
			"not comparable. Omit system_prompt to take the built-in default, or give it a path")
	}
	c, err := claude.New(cfg.Model, systemPrompt, cfg.ModelTuning)
	if err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := c.Validate(ctx); err != nil {
		return nil, fmt.Errorf("backend validation: %w", err)
	}
	return c, nil
}

// errNotImplemented marks a subcommand whose flags are wired but whose body is
// not written yet, so the shell can be exercised without pretending to work.
func errNotImplemented(what string) error {
	return fmt.Errorf("%s: not implemented yet; flags are wired and validated, the report is not built", what)
}
