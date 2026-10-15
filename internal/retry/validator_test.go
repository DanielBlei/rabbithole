// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package retry

import (
	"context"
	"errors"
	"strings"
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
			err := v.Validate(context.Background(), "m", "hint", fn, nil)
			if tt.secondCall {
				err = v.Validate(context.Background(), "m", "hint", fn, nil)
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
	if err := v.Validate(context.Background(), "m", "hint", list, nil); err == nil {
		t.Fatal("Validate() error = nil, want non-nil while down")
	}
	up = true
	if err := v.Validate(context.Background(), "m", "hint", list, nil); err != nil {
		t.Fatalf("Validate() error = %v, want nil once up (failure should not be cached)", err)
	}
}

func TestValidatorFallback(t *testing.T) {
	tests := []struct {
		name       string
		list       func(calls *int) func(context.Context) ([]string, error)
		fallbackOK bool // fallback's verdict when invoked
		secondCall bool // run Validate a second time
		wantErr    bool
		wantList   int
		wantProbe  int
	}{
		{
			name: "model listed skips fallback",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return []string{"m"}, nil }
			},
			wantList:  1,
			wantProbe: 0,
		},
		{
			name: "endpoint unreachable skips fallback",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return nil, errors.New("down") }
			},
			wantErr:   true,
			wantList:  3, // reachability is retried, fallback never reached
			wantProbe: 0,
		},
		{
			name: "unlisted model accepted via fallback",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return []string{"other"}, nil }
			},
			fallbackOK: true,
			wantList:   1,
			wantProbe:  1,
		},
		{
			name: "fallback success is cached",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return []string{"other"}, nil }
			},
			fallbackOK: true,
			secondCall: true,
			wantList:   1,
			wantProbe:  1,
		},
		{
			name: "fallback failure surfaces and is not cached",
			list: func(calls *int) func(context.Context) ([]string, error) {
				return func(context.Context) ([]string, error) { *calls++; return []string{"other"}, nil }
			},
			fallbackOK: false,
			secondCall: true,
			wantErr:    true,
			wantList:   2,
			wantProbe:  2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewValidator("test", 3, time.Millisecond)
			var listCalls, probeCalls int
			list := tt.list(&listCalls)
			fallback := func(context.Context) error {
				probeCalls++
				if !tt.fallbackOK {
					return errors.New("probe rejected")
				}
				return nil
			}
			err := v.Validate(context.Background(), "m", "hint", list, fallback)
			if tt.secondCall {
				err = v.Validate(context.Background(), "m", "hint", list, fallback)
			}
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want non-nil")
			}
			if tt.wantErr && probeCalls > 0 && !strings.Contains(err.Error(), "probe rejected") {
				t.Fatalf("Validate() error = %v, want the fallback error included", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if listCalls != tt.wantList {
				t.Fatalf("list calls = %d, want %d", listCalls, tt.wantList)
			}
			if probeCalls != tt.wantProbe {
				t.Fatalf("fallback calls = %d, want %d", probeCalls, tt.wantProbe)
			}
		})
	}
}
