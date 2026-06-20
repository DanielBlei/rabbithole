package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

func TestDoExhaustsAttempts(t *testing.T) {
	wantErr := errors.New("still down")
	var calls int
	err := Do(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return wantErr
	}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Do() error = %v, want %v", err, wantErr)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDoStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	err := Do(ctx, 5, 50*time.Millisecond, func() error {
		calls++
		if calls == 1 {
			cancel()
		}
		return errors.New("not ready")
	}, nil)
	if err == nil {
		t.Fatal("Do() error = nil, want non-nil")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (should stop waiting once context is cancelled)", calls)
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