// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

//go:build network

package ingest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielBlei/rabbithole/internal/feeds"
	"github.com/DanielBlei/rabbithole/internal/library"
)

const (
	liveDeadline       = 15 * time.Minute
	liveRequestTimeout = 30 * time.Second
	liveProbeBodyLimit = 1 << 20
	liveMaxDomains     = 4
	liveRetryWait      = 10 * time.Second
	liveMaxRetryAfter  = 60 * time.Second
	blockedHint        = " (403, 429 or connection errors can mean the runner is blocked; re-run locally)"
)

// liveJob is one curated source to check.
type liveJob struct {
	file, group, name, url string
	typ                    library.Type
}

// liveOutcome is one attempt at a job. reason is empty when the job passed.
type liveOutcome struct {
	reason     string
	retryable  bool
	retryAfter time.Duration // the probe's Retry-After, 0 when absent
}

// probeResult is the answer to the redirect probe.
type probeResult struct {
	status     int
	location   string // resolved against the request URL, set on 3xx
	retryAfter time.Duration
}

// liveClient does not follow redirects, so the probe sees the first response.
var liveClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// TestLibrarySourcesLive fetches every library source through the app's real fetchers.
// Failures are reported as LIBRARY-FAIL lines, which the CI log greps for.
func TestLibrarySourcesLive(t *testing.T) {
	lib, diags := library.ParseFS(os.DirFS(filepath.Join("..", "..", "library")))
	if err := diags.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lib.Files) == 0 {
		t.Fatal("library has no files")
	}

	jobs, byDomain := liveJobs(lib)
	// Busiest domain first, so the run's wall time follows the longest domain.
	domains := slices.Collect(maps.Keys(byDomain))
	slices.SortFunc(domains, func(a, b string) int {
		if c := cmp.Compare(len(byDomain[b]), len(byDomain[a])); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})

	ctx, cancel := context.WithTimeout(t.Context(), liveDeadline)
	defer cancel()

	reasons := make([]string, len(jobs))
	sem := make(chan struct{}, liveMaxDomains)
	var wg sync.WaitGroup
	for _, domain := range domains {
		sem <- struct{}{}
		wg.Add(1)
		go func(domain string, idx []int) {
			defer wg.Done()
			defer func() { <-sem }()
			pause := domainPause(domain)
			for n, i := range idx {
				if n > 0 {
					sleepCtx(ctx, pause)
				}
				reasons[i] = checkLiveJob(ctx, jobs[i])
			}
		}(domain, byDomain[domain])
	}
	wg.Wait()

	for i, job := range jobs {
		t.Run(job.file+"/"+job.name, func(t *testing.T) {
			if reason := reasons[i]; reason != "" {
				t.Errorf("LIBRARY-FAIL %s %s %q %s: %q%s",
					job.file, job.group, job.name, job.url, reason, liveHint(reason))
			}
		})
	}
}

// liveJobs lists every source in file, group, source order, indexed by registrable domain.
func liveJobs(lib *library.Library) ([]liveJob, map[string][]int) {
	var jobs []liveJob
	byDomain := map[string][]int{}
	for _, f := range lib.Files {
		for _, g := range f.Groups {
			for _, s := range g.Sources {
				domain := registrableDomain(s.URL)
				byDomain[domain] = append(byDomain[domain], len(jobs))
				jobs = append(jobs, liveJob{file: f.Name, group: g.Name, name: s.Name, url: s.URL, typ: f.Type})
			}
		}
	}
	return jobs, byDomain
}

// registrableDomain returns the last two labels of a URL's host, so rss.arxiv.org gives arxiv.org.
// Multi-part suffixes such as co.uk group together, which only affects pacing.
func registrableDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	labels := strings.Split(strings.ToLower(u.Hostname()), ".")
	if len(labels) > 2 {
		labels = labels[len(labels)-2:]
	}
	return strings.Join(labels, ".")
}

// domainPause is the gap between requests to one domain.
func domainPause(domain string) time.Duration {
	if domain == "arxiv.org" {
		return 3 * time.Second
	}
	return time.Second
}

// checkLiveJob runs a job, retrying a failure once. A redirect is never retried.
func checkLiveJob(ctx context.Context, job liveJob) string {
	out := attemptLiveJob(ctx, job)
	if out.reason == "" || !out.retryable {
		return out.reason
	}
	wait := liveRetryWait
	if out.retryAfter > 0 {
		wait = out.retryAfter
	}
	sleepCtx(ctx, wait)
	return attemptLiveJob(ctx, job).reason
}

func attemptLiveJob(ctx context.Context, job liveJob) liveOutcome {
	if job.typ == library.TypeAcademic {
		return academicOutcome(ctx, job)
	}
	return rssOutcome(ctx, job)
}

// rssOutcome probes for redirects, then fetches the feed the way the app does. A feed with no items
// passes, since a daily feed can be empty.
func rssOutcome(ctx context.Context, job liveJob) liveOutcome {
	probe, err := probeRedirect(ctx, job.url)
	if err != nil {
		return liveOutcome{reason: errReason(err), retryable: true}
	}
	switch {
	case probe.status >= 300 && probe.status < 400:
		return liveOutcome{reason: redirectReason(probe.location)}
	case probe.status != http.StatusOK:
		return liveOutcome{
			reason:     fmt.Sprintf("HTTP %d", probe.status),
			retryable:  true,
			retryAfter: probe.retryAfter,
		}
	}
	res := feeds.FetchAll(ctx, []feeds.Source{{Name: job.name, URL: job.url}})[0]
	if res.Err != nil {
		return liveOutcome{reason: errReason(res.Err), retryable: true}
	}
	return liveOutcome{}
}

// academicOutcome runs the saved search through the app's academic fetcher.
func academicOutcome(ctx context.Context, job liveJob) liveOutcome {
	res := fetchAcademic(ctx, []feeds.Source{{Name: job.name, URL: job.url}})[0]
	if res.Err != nil {
		return liveOutcome{reason: errReason(res.Err), retryable: true}
	}
	if len(res.Items) == 0 {
		return liveOutcome{reason: "no items", retryable: true}
	}
	if registrableDomain(job.url) == "arxiv.org" && !hasArXivPaper(res.Items) {
		return liveOutcome{reason: "no arXiv paper in the results", retryable: true}
	}
	return liveOutcome{}
}

// probeRedirect sends the request gofeed sends, so the probe sees what the app sees.
func probeRedirect(ctx context.Context, rawURL string) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, liveRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("User-Agent", "Gofeed/1.0")
	resp, err := liveClient.Do(req)
	if err != nil {
		return probeResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, liveProbeBodyLimit)

	res := probeResult{
		status:     resp.StatusCode,
		retryAfter: retryAfter(resp.Header.Get("Retry-After")),
	}
	if loc, err := resp.Location(); err == nil {
		res.location = loc.String()
	}
	return res, nil
}

// errReason is the failure text; a deadline is named "timeout" so the hint can match it.
func errReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout: " + err.Error()
	}
	return err.Error()
}

func redirectReason(location string) string {
	if location == "" {
		return "redirects without a Location header"
	}
	return fmt.Sprintf("redirects to %s, store that URL instead", location)
}

// retryAfter reads a Retry-After value in seconds, capped at liveMaxRetryAfter. It is 0 when absent.
func retryAfter(value string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, liveMaxRetryAfter)
}

func hasArXivPaper(items []feeds.Item) bool {
	for _, it := range items {
		if strings.Contains(it.Link, "arxiv.org/abs/") {
			return true
		}
	}
	return false
}

// blockedStatus matches the HTTP statuses a blocking runner gets, as whole numbers.
var blockedStatus = regexp.MustCompile(`\b(403|429)\b`)

// liveHint names a likely blocked-runner cause in the failure reason.
func liveHint(reason string) string {
	if blockedStatus.MatchString(reason) {
		return blockedHint
	}
	lower := strings.ToLower(reason)
	for _, word := range []string{"timeout", "connection", "no such host"} {
		if strings.Contains(lower, word) {
			return blockedHint
		}
	}
	return ""
}

// sleepCtx pauses for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
