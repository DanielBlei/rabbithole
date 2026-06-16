// Package inference resolves the configured backend into a rank.Scorer.
package inference

import (
	"context"
	"fmt"

	"github.com/DanielBlei/ai-searcher/internal/ollama"
	"github.com/DanielBlei/ai-searcher/internal/rank"
	"github.com/DanielBlei/ai-searcher/internal/vllm"
)

// Config carries the parameters needed to build a backend client.
type Config struct {
	Provider string // ollama | vllm | heuristic
	ChatHost string
	Model    string
	APIKey   string
	Think    bool // enable model reasoning mode during scoring
}

// Resolve constructs and validates the Scorer for the configured provider.
func Resolve(ctx context.Context, cfg Config) (rank.Scorer, error) {
	var (
		s   rank.Scorer
		err error
	)
	switch cfg.Provider {
	case "ollama":
		s, err = ollama.New(cfg.ChatHost, cfg.Model, cfg.APIKey, cfg.Think)
	case "vllm":
		s, err = vllm.New(cfg.ChatHost, cfg.Model, cfg.APIKey, cfg.Think)
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
