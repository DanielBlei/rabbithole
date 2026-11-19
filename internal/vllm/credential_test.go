// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package vllm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/rank"
)

type effortProbeLog struct {
	mu   sync.Mutex
	seen map[string][]string
}

func newEffortProbeLog() *effortProbeLog {
	return &effortProbeLog{seen: map[string][]string{}}
}

func (p *effortProbeLog) add(token, effort string) {
	p.mu.Lock()
	p.seen[token] = append(p.seen[token], effort)
	p.mu.Unlock()
}

func (p *effortProbeLog) get(token string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen[token]...)
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func writeCompletion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`))
}

func writeModels(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":[{"id":"gpt"}]}`))
}

// TestEffortCapabilityIsCredentialScoped is the Blocker 1 reproduction for an
// OpenAI-compatible gateway: key-a accepts reasoning_effort high, key-b rejects
// it. Cache reuse across those credentials would make key-b's dive fail.
func TestEffortCapabilityIsCredentialScoped(t *testing.T) {
	withFastValidateRetry(t)
	resetEffortCapabilities()
	probes := newEffortProbeLog()
	var synthesisMu sync.Mutex
	synthesis := map[string]map[string]any{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeModels(w)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		token := bearerToken(r)
		if isEffortProbe(body) {
			effort, _ := body["reasoning_effort"].(string)
			probes.add(token, effort)
			if token != "key-a" && effort == "high" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unsupported reasoning_effort"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
			return
		}
		synthesisMu.Lock()
		synthesis[token] = body
		synthesisMu.Unlock()
		writeCompletion(w)
	}))
	defer srv.Close()

	scoringA, err := New(OpenAI, srv.URL, "gpt", "key-a", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoringA.Validate(t.Context()); err != nil {
		t.Fatalf("Validate key-a: %v", err)
	}
	if scoringA.sendEffort != "high" {
		t.Fatalf("key-a effort = %q, want high", scoringA.sendEffort)
	}

	scoringB, err := New(OpenAI, srv.URL, "gpt", "key-b", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoringB.Validate(t.Context()); err != nil {
		t.Fatalf("Validate key-b: %v", err)
	}
	if scoringB.sendEffort != "" {
		t.Fatalf("key-b inherited key-a's effort %q, want the no-field fallback", scoringB.sendEffort)
	}

	// Reuse within a credential, on the brief path as well as scoring.
	briefA, err := New(OpenAI, srv.URL, "gpt", "key-a", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if briefA.sendEffort != "high" || !briefA.effortResolved {
		t.Fatalf("key-a brief effort = %q (resolved=%v), want cached high",
			briefA.sendEffort, briefA.effortResolved)
	}
	if err := briefA.NegotiateEffort(t.Context()); err != nil {
		t.Fatal(err)
	}
	briefB, err := New(OpenAI, srv.URL, "gpt", "key-b", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if briefB.sendEffort != "" || !briefB.effortResolved {
		t.Fatalf("key-b brief effort = %q (resolved=%v), want cached fallback",
			briefB.sendEffort, briefB.effortResolved)
	}
	if err := briefB.NegotiateEffort(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := briefB.ChatJSON(t.Context(), "system", "user",
		json.RawMessage(`{"type":"object"}`), 64); err != nil {
		t.Fatalf("key-b brief ChatJSON: %v", err)
	}
	synthesisMu.Lock()
	keyBRequest := synthesis["key-b"]
	synthesisMu.Unlock()
	if effort, ok := keyBRequest["reasoning_effort"]; ok {
		t.Fatalf("key-b synthesis sent reasoning_effort = %v, want omitted", effort)
	}

	// A credential first seen later negotiates independently.
	scoringC, err := New(OpenAI, srv.URL, "gpt", "key-c", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoringC.Validate(t.Context()); err != nil {
		t.Fatalf("Validate key-c: %v", err)
	}
	if scoringC.sendEffort != "" {
		t.Fatalf("key-c effort = %q, want the fallback", scoringC.sendEffort)
	}

	if got := probes.get("key-a"); !slices.Equal(got, []string{"high"}) {
		t.Errorf("key-a probes = %q, want [high]", got)
	}
	if got := probes.get("key-b"); !slices.Equal(got, []string{"high", ""}) {
		t.Errorf("key-b probes = %q, want [high \"\"]", got)
	}
	if got := probes.get("key-c"); !slices.Equal(got, []string{"high", ""}) {
		t.Errorf("key-c probes = %q, want [high \"\"]", got)
	}
}

// TestEffortCapabilityEmptyCredential pins deterministic sharing for a
// credential-less endpoint.
func TestEffortCapabilityEmptyCredential(t *testing.T) {
	withFastValidateRetry(t)
	resetEffortCapabilities()
	probes := newEffortProbeLog()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeModels(w)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if isEffortProbe(body) {
			effort, _ := body["reasoning_effort"].(string)
			probes.add(bearerToken(r), effort)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		writeCompletion(w)
	}))
	defer srv.Close()

	for i := 0; i < 2; i++ {
		c, err := New(OpenAI, srv.URL, "gpt", "", true, "high", rank.ModelTuning{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(t.Context()); err != nil {
			t.Fatalf("Validate %d: %v", i, err)
		}
		if c.sendEffort != "high" {
			t.Fatalf("client %d effort = %q, want high", i, c.sendEffort)
		}
	}
	if got := probes.get(""); !slices.Equal(got, []string{"high"}) {
		t.Fatalf("empty-credential probes = %q, want one high probe", got)
	}
}

// TestEffortCapabilityConcurrentSameCredential pins the single-flight guard.
func TestEffortCapabilityConcurrentSameCredential(t *testing.T) {
	resetEffortCapabilities()
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
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	first, err := New(OpenAI, srv.URL, "gpt", "key-a", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(OpenAI, srv.URL, "gpt", "key-a", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = first.NegotiateEffort(t.Context()) }()
	<-entered
	go func() { defer wg.Done(); _ = second.NegotiateEffort(t.Context()) }()
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&probes); got != 1 {
		t.Fatalf("probes = %d with one credential, want 1", got)
	}
	if first.sendEffort != "high" || second.sendEffort != "high" {
		t.Fatalf("efforts = %q/%q, want both high", first.sendEffort, second.sendEffort)
	}
}

// TestEffortCapabilityConcurrentDifferentCredentials pins independent
// negotiation for two credentials in flight together.
func TestEffortCapabilityConcurrentDifferentCredentials(t *testing.T) {
	resetEffortCapabilities()
	probes := newEffortProbeLog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		token := bearerToken(r)
		effort, _ := body["reasoning_effort"].(string)
		probes.add(token, effort)
		if token != "key-a" && effort == "high" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unsupported reasoning_effort"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a, err := New(OpenAI, srv.URL, "gpt", "key-a", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(OpenAI, srv.URL, "gpt", "key-b", true, "high", rank.ModelTuning{}, "")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = a.NegotiateEffort(t.Context()) }()
	go func() { defer wg.Done(); _ = b.NegotiateEffort(t.Context()) }()
	wg.Wait()

	if a.sendEffort != "high" {
		t.Fatalf("key-a effort = %q, want high", a.sendEffort)
	}
	if b.sendEffort != "" {
		t.Fatalf("key-b effort = %q, want the fallback", b.sendEffort)
	}
	if got := probes.get("key-a"); !slices.Equal(got, []string{"high"}) {
		t.Errorf("key-a probes = %q, want [high]", got)
	}
	if got := probes.get("key-b"); !slices.Equal(got, []string{"high", ""}) {
		t.Errorf("key-b probes = %q, want [high \"\"]", got)
	}
}
