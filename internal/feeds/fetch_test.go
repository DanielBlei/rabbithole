// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package feeds

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DanielBlei/rabbithole/internal/httpclient"
)

const sampleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
  <title>Test Feed</title>
  <item>
    <title>Scaling vLLM inference</title>
    <link>https://example.com/vllm</link>
    <guid>https://example.com/vllm</guid>
    <description><![CDATA[<p>How we cut <b>latency</b> with continuous batching.</p>]]></description>
    <pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate>
  </item>
  <item>
    <title>Intro to Python</title>
    <link>https://example.com/python</link>
    <guid>https://example.com/python</guid>
    <description>Beginner tutorial.</description>
  </item>
</channel></rss>`

func TestFetchAllParsesItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	results := FetchAll(context.Background(), srv.Client(), []Source{{Name: "Test", URL: srv.URL}})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("fetch error: %v", results[0].Err)
	}
	items := results[0].Items
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	first := items[0]
	if first.Title != "Scaling vLLM inference" {
		t.Errorf("title = %q", first.Title)
	}
	if first.Source != "Test" {
		t.Errorf("source = %q", first.Source)
	}
	if first.ID == "" {
		t.Error("ID is empty")
	}
	// HTML should be stripped from the summary.
	if want := "How we cut latency with continuous batching."; first.Summary != want {
		t.Errorf("summary = %q, want %q", first.Summary, want)
	}
	if first.Published.IsZero() {
		t.Error("expected a parsed publish date")
	}
}

func TestFetchAllSkipsBadFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	results := FetchAll(context.Background(), srv.Client(), []Source{{Name: "Bad", URL: srv.URL}})
	if n := len(results[0].Items); n != 0 {
		t.Fatalf("got %d items, want 0 from a failing feed", n)
	}
	// The failure must be reported rather than swallowed — feed health is
	// built from it.
	if len(results) != 1 || results[0].Err == nil {
		t.Fatal("expected a Result carrying the fetch error")
	}
	if results[0].Source.Name != "Bad" {
		t.Errorf("result source = %q, want Bad", results[0].Source.Name)
	}
}

func TestFetchAllReturnsResultsInSourceOrder(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	// Fetches run concurrently; results must still come back positionally so
	// a caller can pair them with the sources it passed in.
	sources := []Source{{Name: "A", URL: bad.URL}, {Name: "B", URL: good.URL}, {Name: "C", URL: bad.URL}}
	results := FetchAll(context.Background(), good.Client(), sources)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, want := range []string{"A", "B", "C"} {
		if results[i].Source.Name != want {
			t.Errorf("results[%d].Source.Name = %q, want %q", i, results[i].Source.Name, want)
		}
	}
	if results[1].Err != nil {
		t.Errorf("B should have succeeded: %v", results[1].Err)
	}
	if results[0].Err == nil || results[2].Err == nil {
		t.Error("A and C should have failed")
	}
}

func TestFetchAllRefusesLoopbackByDefault(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	for _, client := range []*http.Client{nil, NewClient("1.2.3")} {
		res := FetchAll(context.Background(), client, []Source{{Name: "Local", URL: srv.URL}})[0]
		if !errors.Is(res.Err, httpclient.ErrBlockedAddress) {
			t.Errorf("err = %v, want ErrBlockedAddress", res.Err)
		}
	}
}

func TestFetchAllRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Big</title><description>`))
		chunk := []byte(strings.Repeat("x", 1<<20))
		for range MaxFeedBytes/len(chunk) + 1 {
			_, _ = w.Write(chunk)
		}
		_, _ = w.Write([]byte(`</description></channel></rss>`))
	}))
	defer srv.Close()

	res := FetchAll(context.Background(), srv.Client(), []Source{{Name: "Big", URL: srv.URL}})[0]
	if !errors.Is(res.Err, httpclient.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", res.Err)
	}
	if len(res.Items) != 0 {
		t.Errorf("got %d items from an oversized feed", len(res.Items))
	}
}

func TestNewClientSendsUserAgentAndHonoursOptIn(t *testing.T) {
	t.Setenv(httpclient.AllowPrivateEnv, "1")
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	res := FetchAll(context.Background(), NewClient("1.2.3"), []Source{{Name: "UA", URL: srv.URL}})[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if want := "rabbithole/1.2.3 (+https://github.com/DanielBlei/rabbithole)"; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

func TestMakeIDStable(t *testing.T) {
	a := makeID("guid-1", "https://x.com/a")
	b := makeID("guid-1", "https://x.com/different")
	if a != b {
		t.Error("ID should be derived from GUID when present")
	}
	c := makeID("", "https://x.com/a")
	if c == a {
		t.Error("link-derived ID should differ from guid-derived ID")
	}
}
