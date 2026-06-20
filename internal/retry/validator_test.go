package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidatorCachesSuccess(t *testing.T) {
	v := NewValidator("test", 3, time.Millisecond)
	var calls int
	list := func(context.Context) ([]string, error) {
		calls++
		return []string{"m"}, nil
	}
	for range 2 {
		if err := v.Validate(context.Background(), "m", "hint", list); err != nil {
			t.Fatalf("Validate() error = %v, want nil", err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (success should be cached)", calls)
	}
}

func TestValidatorRetriesListOnError(t *testing.T) {
	v := NewValidator("test", 3, time.Millisecond)
	var calls int
	list := func(context.Context) ([]string, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("not up yet")
		}
		return []string{"m"}, nil
	}
	if err := v.Validate(context.Background(), "m", "hint", list); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestValidatorDoesNotCacheFailure(t *testing.T) {
	v := NewValidator("test", 1, time.Millisecond)
	var up bool
	list := func(context.Context) ([]string, error) {
		if !up {
			return nil, errors.New("down")
		}
		return []string{"m"}, nil
	}
	if err := v.Validate(context.Background(), "m", "hint", list); err == nil {
		t.Fatal("Validate() error = nil, want non-nil while down")
	}
	up = true
	if err := v.Validate(context.Background(), "m", "hint", list); err != nil {
		t.Fatalf("Validate() error = %v, want nil once up (failure should not be cached)", err)
	}
}

func TestValidatorMissingModelDoesNotRetryList(t *testing.T) {
	v := NewValidator("test", 3, time.Millisecond)
	var calls int
	list := func(context.Context) ([]string, error) {
		calls++
		return []string{"other"}, nil
	}
	if err := v.Validate(context.Background(), "m", "hint", list); err == nil {
		t.Fatal("Validate() error = nil, want non-nil")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (missing model should fail fast, not retry)", calls)
	}
}