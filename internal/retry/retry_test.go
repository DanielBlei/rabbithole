// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errStillDown = errors.New("still down")

func TestDoSucceedsAfterFailures(t *testing.T) {
	var calls int
	var retries []int
	err := Do(context.Background(), 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("not ready")
		}
		return nil
	}, func(attempt int, _ error, _ time.Duration) {
		retries = append(retries, attempt)
	})
	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if want := []int{1, 2}; !equal(retries, want) {
		t.Fatalf("retries = %v, want %v", retries, want)
	}
}

func TestDoFailureModes(t *testing.T) {
	tests := []struct {
		name      string
		attempts  int
		delay     time.Duration
		fn        func(calls *int, cancel context.CancelFunc) error
		wantCalls int
		wantErr   error
	}{
		{
			name:     "exhausts attempts",
			attempts: 3,
			delay:    time.Millisecond,
			fn: func(calls *int, _ context.CancelFunc) error {
				*calls++
				return errStillDown
			},
			wantCalls: 3,
			wantErr:   errStillDown,
		},
		{
			name:     "stops on context cancel",
			attempts: 5,
			delay:    50 * time.Millisecond,
			fn: func(calls *int, cancel context.CancelFunc) error {
				*calls++
				if *calls == 1 {
					cancel()
				}
				return errStillDown
			},
			wantCalls: 1,
			// The context error, not fn's, so a cancelled run is
			// distinguishable from one that used up its attempts.
			wantErr: context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var calls int
			err := Do(ctx, tt.attempts, tt.delay, func() error { return tt.fn(&calls, cancel) }, nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Do() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if calls != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestDoSucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 3, time.Hour, func() error {
		calls++
		return nil
	}, func(int, error, time.Duration) {
		t.Fatal("onRetry should not be called when the first attempt succeeds")
	})
	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
