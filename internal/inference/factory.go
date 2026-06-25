// Package inference resolves the configured backend into a rank.Scorer.
package inference

import (
	"context"
	"fmt"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/ollama"
	"github.com/DanielBlei/rabbithole/internal/rank"
	"github.com/DanielBlei/rabbithole/internal/vllm"
)

// Resolve constructs and validates the Scorer for the configured provider.
// think is passed separately from cfg because a single run can override the
// configured default (e.g. via --no-think) without editing the config.
func Resolve(ctx context.Context, cfg config.InferenceConfig, think bool) (rank.Scorer, error) {
	var (
		s   rank.Scorer
		err error
	)
	switch cfg.Provider {
	case "ollama":
		s, err = ollama.New(cfg.Host, cfg.Model, cfg.APIKey, think)
	case "vllm":
		s, err = vllm.New(cfg.Host, cfg.Model, cfg.APIKey, think)
	case "heuristic":
		s = rank.NewHeuristic()
	default:
		return nil, fmt.Errorf("unknown provider %q, must be ollama, vllm or heuristic", cfg.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := s.Validate(ctx); err != nil {
		return nil, fmt.Errorf("backend validation: %w", err)
	}
	return s, nil
}
