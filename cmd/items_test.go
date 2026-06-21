package cmd

import (
	"errors"
	"slices"
	"testing"
)

func TestApplyToEach(t *testing.T) {
	tests := []struct {
		name        string
		identifiers []string
		failOn      map[string]bool
		wantErr     bool
		wantCalls   []string
	}{
		{
			name:        "all succeed",
			identifiers: []string{"a", "b", "c"},
			failOn:      map[string]bool{},
			wantCalls:   []string{"a", "b", "c"},
		},
		{
			name:        "one fails, the rest still run",
			identifiers: []string{"a", "b", "c"},
			failOn:      map[string]bool{"b": true},
			wantErr:     true,
			wantCalls:   []string{"a", "b", "c"},
		},
		{
			name:        "all fail",
			identifiers: []string{"a", "b"},
			failOn:      map[string]bool{"a": true, "b": true},
			wantErr:     true,
			wantCalls:   []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := applyToEach(tt.identifiers, func(id string) error {
				calls = append(calls, id)
				if tt.failOn[id] {
					return errors.New("boom")
				}
				return nil
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("applyToEach() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}
