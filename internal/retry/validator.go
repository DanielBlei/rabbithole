package retry

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Validator confirms a backend is reachable and serving a given model. It
// retries the reachability probe with backoff (the backend may still be
// starting up) but does not retry a model-presence miss, since that's a
// misconfiguration, not a transient state.
//
// A successful Validate is cached so repeat calls are free. A failed one is
// not: the next call starts over, so a backend that's still starting up (or
// briefly down) doesn't get stuck on a stale error for the life of a
// long-running process.
type Validator struct {
	name     string // log/error prefix, e.g. "ollama" or "vllm"
	attempts int
	backoff  time.Duration

	mu sync.Mutex
	ok bool
}

// NewValidator builds a Validator that retries its reachability probe up to
// attempts times, waiting backoff (doubling each time) between tries.
func NewValidator(name string, attempts int, backoff time.Duration) *Validator {
	return &Validator{name: name, attempts: attempts, backoff: backoff}
}

// Validate confirms model is present among list's results. hint is appended
// to the error when the backend is reachable but model isn't found there.
func (v *Validator) Validate(
	ctx context.Context,
	model, hint string,
	list func(ctx context.Context) ([]string, error),
) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ok {
		return nil
	}

	logger := zerolog.Ctx(ctx)
	var models []string
	err := Do(ctx, v.attempts, v.backoff, func() error {
		m, err := list(ctx)
		if err != nil {
			return err
		}
		models = m
		return nil
	}, func(attempt int, err error, delay time.Duration) {
		logger.Debug().Str("backend", v.name).Int("attempt", attempt).Err(err).
			Str("retry_in", delay.String()).Msg(v.name + ": not reachable yet, retrying")
	})
	if err != nil {
		return err
	}

	if slices.Contains(models, model) {
		v.ok = true
		return nil
	}
	return fmt.Errorf("model %q not found on %s (%s)", model, v.name, hint)
}
