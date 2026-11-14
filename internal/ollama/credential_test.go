// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

// probeLog records think probes per credential so a test can prove one
// credential's negotiation never answers for another.
type probeLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func newProbeLog() *probeLog { return &probeLog{seen: map[string]int{}} }

func (p *probeLog) add(token string) {
	p.mu.Lock()
	p.seen[token]++
	p.mu.Unlock()
}

func (p *probeLog) count(token string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen[token]
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func writeThinkResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{}"},"done":true}` + "\n"))
}

// TestThinkCapabilityIsCredentialScoped is the Blocker 1 reproduction: the
// same endpoint and model accept think mode under one API key and reject it
// under another. Neither direction may reuse the other's answer.
func TestThinkCapabilityIsCredentialScoped(t *testing.T) {
	withFastValidateRetry(t)
	resetThinkCapabilities()
	probes := newProbeLog()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"model":"llama3:latest"}]}`))
			return
		}
		var req struct {
			Think *bool `json:"think"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Think != nil && *req.Think {
			token := bearerToken(r)
			probes.add(token)
			if token != "key-a" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"\"llama3:latest\" does not support thinking"}`))
				return
			}
		}
		writeThinkResponse(w)
	}))
	defer srv.Close()

	scoringA, err := New(srv.URL, "llama3:latest", "key-a", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoringA.Validate(t.Context()); err != nil {
		t.Fatalf("Validate key-a: %v", err)
	}
	if !scoringA.think {
		t.Fatal("key-a supports think mode but it was disabled")
	}

	scoringB, err := New(srv.URL, "llama3:latest", "key-b", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoringB.Validate(t.Context()); err != nil {
		t.Fatalf("Validate key-b: %v", err)
	}
	if scoringB.think {
		t.Fatal("key-b inherited key-a's supported-think capability")
	}

	// Reuse within one credential, on both the scoring and brief paths.
	briefA, err := New(srv.URL, "llama3:latest", "key-a", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !briefA.think {
		t.Fatal("key-a brief did not reuse its supported-think capability")
	}
	briefA.NegotiateThink(t.Context())
	briefB, err := New(srv.URL, "llama3:latest", "key-b", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if briefB.think {
		t.Fatal("key-b brief did not reuse its unsupported-think capability")
	}
	briefB.NegotiateThink(t.Context())

	// A credential first seen later negotiates independently.
	scoringC, err := New(srv.URL, "llama3:latest", "key-c", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoringC.Validate(t.Context()); err != nil {
		t.Fatalf("Validate key-c: %v", err)
	}
	if scoringC.think {
		t.Fatal("key-c inherited another credential's capability")
	}

	if got := probes.count("key-a"); got != 1 {
		t.Errorf("key-a probes = %d, want 1", got)
	}
	if got := probes.count("key-b"); got != 1 {
		t.Errorf("key-b probes = %d, want 1", got)
	}
	if got := probes.count("key-c"); got != 1 {
		t.Errorf("key-c probes = %d, want 1", got)
	}
}

// TestThinkCapabilityEmptyCredential pins deterministic behavior when no
// credential is configured: credential-less clients share one cache entry.
func TestThinkCapabilityEmptyCredential(t *testing.T) {
	withFastValidateRetry(t)
	resetThinkCapabilities()
	probes := newProbeLog()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"model":"llama3:latest"}]}`))
			return
		}
		var req struct {
			Think *bool `json:"think"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Think != nil && *req.Think {
			probes.add(bearerToken(r))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"\"llama3:latest\" does not support thinking"}`))
			return
		}
		writeThinkResponse(w)
	}))
	defer srv.Close()

	for i := 0; i < 2; i++ {
		c, err := New(srv.URL, "llama3:latest", "", true, rank.ModelTuning{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(t.Context()); err != nil {
			t.Fatalf("Validate %d: %v", i, err)
		}
		if c.think {
			t.Fatalf("client %d think = true, want the cached unsupported answer", i)
		}
	}
	if got := probes.count(""); got != 1 {
		t.Fatalf("empty-credential probes = %d, want 1", got)
	}
}

// TestThinkCapabilityConcurrentSameCredential pins the single-flight guard: a
// second client waiting on the same credential must reuse the first probe.
func TestThinkCapabilityConcurrentSameCredential(t *testing.T) {
	resetThinkCapabilities()
	var probes int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probes, 1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"\"llama3:latest\" does not support thinking"}`))
	}))
	defer srv.Close()

	first, err := New(srv.URL, "llama3:latest", "key-a", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(srv.URL, "llama3:latest", "key-a", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); first.NegotiateThink(t.Context()) }()
	<-entered
	go func() { defer wg.Done(); second.NegotiateThink(t.Context()) }()
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&probes); got != 1 {
		t.Fatalf("probes = %d with one credential, want 1", got)
	}
	if first.think || second.think {
		t.Fatalf("think = %v/%v, want both disabled", first.think, second.think)
	}
}

// TestThinkCapabilityConcurrentDifferentCredentials pins independent
// negotiation: two credentials may be in flight without sharing an answer.
func TestThinkCapabilityConcurrentDifferentCredentials(t *testing.T) {
	resetThinkCapabilities()
	probes := newProbeLog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		probes.add(token)
		if token == "key-a" {
			writeThinkResponse(w)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"\"llama3:latest\" does not support thinking"}`))
	}))
	defer srv.Close()

	a, err := New(srv.URL, "llama3:latest", "key-a", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(srv.URL, "llama3:latest", "key-b", true, rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.NegotiateThink(t.Context()) }()
	go func() { defer wg.Done(); b.NegotiateThink(t.Context()) }()
	wg.Wait()

	if !a.think {
		t.Fatal("key-a lost its supported-think capability")
	}
	if b.think {
		t.Fatal("key-b inherited key-a's supported-think capability")
	}
	if probes.count("key-a") != 1 || probes.count("key-b") != 1 {
		t.Fatalf("probes = a:%d b:%d, want one each", probes.count("key-a"), probes.count("key-b"))
	}
}
