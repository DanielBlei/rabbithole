// Package retry provides exponential backoff for operations against a
// dependency that may still be starting up (e.g. a local Ollama/vLLM server).
package retry

import (
	"context"
	"time"
)

// Do calls fn until it succeeds or attempts is exhausted. Between attempts it
// waits delay, then doubles delay for the next wait. onRetry, if non-nil, is
// invoked after a failed attempt (other than the last) with the attempt
// number that just failed, its error, and the upcoming delay.
func Do(ctx context.Context, attempts int, delay time.Duration, fn func() error, onRetry func(attempt int, err error, delay time.Duration)) error {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == attempts {
			return err
		}
		if onRetry != nil {
			onRetry(attempt, err, delay)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return err
		}
		delay *= 2
	}
	return err
}