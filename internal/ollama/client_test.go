package ollama

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func withFastValidateRetry(t *testing.T) {
	origAttempts, origBackoff := validateAttempts, validateBackoff
	validateAttempts = 3
	validateBackoff = time.Millisecond
	t.Cleanup(func() {
		validateAttempts = origAttempts
		validateBackoff = origBackoff
	})
}

func TestValidate(t *testing.T) {
	withFastValidateRetry(t)

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "model present succeeds", body: `{"models":[{"model":"llama3:latest"}]}`},
		{name: "model missing fails", body: `{"models":[{"model":"other:latest"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3:latest", "", false)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = c.Validate(t.Context())
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want non-nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestListModelNames(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		statusCode int // 0 means 200
		want       []string
		wantErr    string // substring; empty means no error
	}{
		{
			name: "decodes and normalizes model list",
			body: `{"models":[{"model":"llama3:latest"},{"model":"mistral"}]}`,
			want: []string{"llama3:latest", "mistral:latest"},
		},
		{
			name:       "server error surfaces as error",
			statusCode: http.StatusServiceUnavailable,
			wantErr:    "connect to ollama",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.statusCode != 0 {
					w.WriteHeader(tt.statusCode)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "llama3:latest", "", false)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			got, err := c.listModelNames(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("listModelNames() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("listModelNames() error = %v, want nil", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("listModelNames() = %v, want %v", got, tt.want)
			}
		})
	}
}
