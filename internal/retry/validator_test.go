package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidatorValidate(t *testing.T) {
	tests := []struct {
		name       string
		secondCall bool
		list       func(calls *int) func(context.Context) ([]string, error)
		wantErr    bool
		wantCalls  int
	}{
		{
			name:       "caches success",
			secondCall: true,
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return []string{"m"}, nil }
			},
			wantCalls: 1,
		},
		{
			name: "retries list on error",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) {
					*calls++
					if *calls < 3 {
						return nil, errors.New("not up yet")
					}
					return []string{"m"}, nil
				}
			},
			wantCalls: 3,
		},
		{
			name: "missing model does not retry list",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return []string{"other"}, nil }
			},
			wantErr:   true,
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewValidator("test", 3, time.Millisecond)
			var calls int
			fn := tt.list(&calls)
			err := v.Validate(context.Background(), "m", "hint", fn)
			if tt.secondCall {
				err = v.Validate(context.Background(), "m", "hint", fn)
			}
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want non-nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if calls != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tt.wantCalls)
			}
		})
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
