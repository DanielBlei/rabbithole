// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package cluster groups feed items that describe the same development.
package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/rs/zerolog"
)

const (
	DefaultEmbeddingThreshold    = 0.82
	DefaultMaxComparisonsPerItem = 24
	DefaultMaxVerifications      = 64
	DefaultMaxItems              = 500
	minCrossSourceTitleTokens    = 5
	fallbackJaccardThreshold     = 0.55
	maxSummaryForClustering      = 1200
)

// Item is one feed item considered for clustering.
type Item struct {
	ID        string
	Source    string
	Title     string
	Link      string
	Summary   string
	Published time.Time
	Score     int
}

// Cluster is one persisted story group.
type Cluster struct {
	ID                   string
	RepresentativeItemID string
	Method               string
	Confidence           float64
	Items                []ClusterItem
}

// ClusterItem is one member of a story cluster.
type ClusterItem struct {
	ItemID   string
	Position int
}

// Embedder turns item text into vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// Verifier decides whether two items describe the same development.
type Verifier interface {
	SameDevelopment(ctx context.Context, a, b Item) (bool, error)
}

// Options bounds one clustering run.
type Options struct {
	Embedder              Embedder
	Verifier              Verifier
	EmbeddingThreshold    float64
	MaxComparisonsPerItem int
	MaxVerifications      int
	MaxItems              int
}

func (o Options) withDefaults() Options {
	if o.EmbeddingThreshold <= 0 {
		o.EmbeddingThreshold = DefaultEmbeddingThreshold
	}
	if o.MaxComparisonsPerItem <= 0 {
		o.MaxComparisonsPerItem = DefaultMaxComparisonsPerItem
	}
	if o.MaxVerifications <= 0 {
		o.MaxVerifications = DefaultMaxVerifications
	}
	if o.MaxItems <= 0 {
		o.MaxItems = DefaultMaxItems
	}
	return o
}

// Stats reports the work one run performed.
type Stats struct {
	Input                int
	Clusters             int
	Comparisons          int
	Verifications        int
	Embeddings           int
	EmbeddingFallback    bool
	VerificationFallback bool
}

// Build groups items into story clusters. Exact duplicates are grouped first
// without model calls. Blocking limits later comparisons, and every non-exact
// merge must pass verification.
func Build(ctx context.Context, items []Item, opts Options) ([]Cluster, Stats, error) {
	opts = opts.withDefaults()
	logger := zerolog.Ctx(ctx)
	items = prepareItems(items, opts.MaxItems)
	stats := Stats{Input: len(items)}
	if len(items) < 2 {
		return nil, stats, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, stats, err
	}

	groups := exactGroups(items)
	embeddings := loadEmbeddings(ctx, items, opts, &stats, logger)

	candidates := newCandidateIndex()
	clusters := make([]*workingCluster, 0, len(groups))
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		merged := -1
		var mergeSimilarity float64
		for _, candidate := range candidates.forItems(items, group, opts.MaxComparisonsPerItem) {
			other := clusters[candidate]
			sim, similar := similarity(items, group, other.members, embeddings, opts)
			stats.Comparisons++
			if !similar {
				continue
			}
			verified, usedVerifier := verify(ctx, opts, items[group[0]], items[other.members[0]], &stats, logger)
			if verified {
				merged, mergeSimilarity = candidate, sim
				if usedVerifier {
					other.method = "verified"
				} else if other.method == "" || other.method == "exact" {
					other.method = "conservative"
				}
				break
			}
		}
		if merged >= 0 {
			clusters[merged].members = append(clusters[merged].members, group...)
			if mergeSimilarity > clusters[merged].confidence {
				clusters[merged].confidence = mergeSimilarity
			}
			candidates.add(clusters[merged].id, items, group)
			continue
		}
		method := ""
		confidence := 0.0
		if len(group) > 1 {
			method, confidence = "exact", 1
		}
		clusters = append(clusters, &workingCluster{
			id: len(clusters), members: append([]int(nil), group...),
			method: method, confidence: confidence,
		})
		candidates.add(len(clusters)-1, items, group)
	}

	out := make([]Cluster, 0, len(clusters))
	for _, c := range clusters {
		if len(c.members) < 2 {
			continue
		}
		itemsOut := make([]ClusterItem, len(c.members))
		for i, idx := range c.members {
			itemsOut[i] = ClusterItem{ItemID: items[idx].ID, Position: i}
		}
		out = append(out, Cluster{
			ID:                   clusterID(itemsOut),
			RepresentativeItemID: items[c.members[0]].ID,
			Method:               c.method,
			Confidence:           c.confidence,
			Items:                itemsOut,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	stats.Clusters = len(out)
	return out, stats, nil
}

type workingCluster struct {
	id         int
	members    []int
	method     string
	confidence float64
}

func prepareItems(items []Item, limit int) []Item {
	seen := make(map[string]struct{}, len(items))
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Title) == "" {
			continue
		}
		if _, ok := seen[it.ID]; ok {
			continue
		}
		seen[it.ID] = struct{}{}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		az, bz := a.Published.IsZero(), b.Published.IsZero()
		if az != bz {
			return !az
		}
		if !a.Published.Equal(b.Published) {
			return a.Published.After(b.Published)
		}
		return a.ID < b.ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func exactGroups(items []Item) [][]int {
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
			return
		}
		parent[ra] = rb
	}
	owner := make(map[string]int, len(items)*2)
	add := func(i int, key string) {
		if key == "" {
			return
		}
		if first, ok := owner[key]; ok {
			union(i, first)
			return
		}
		owner[key] = i
	}
	for i, it := range items {
		add(i, "u:"+normalizeURL(it.Link))
		title := normalizeTitle(it.Title)
		add(i, "ts:"+title+"\x00"+normalizeSource(it.Source))
		if len(significantTokens(it.Title)) >= minCrossSourceTitleTokens {
			add(i, "t:"+title)
		}
	}
	grouped := make(map[int][]int, len(items))
	for i := range items {
		root := find(i)
		grouped[root] = append(grouped[root], i)
	}
	groups := make([][]int, 0, len(grouped))
	for _, g := range grouped {
		sort.Ints(g)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

func loadEmbeddings(ctx context.Context, items []Item, opts Options, stats *Stats, logger *zerolog.Logger) [][]float64 {
	if opts.Embedder == nil {
		return nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.Title + "\n" + excerpt(it.Summary, maxSummaryForClustering)
	}
	got, err := opts.Embedder.Embed(ctx, texts)
	if err != nil {
		stats.EmbeddingFallback = true
		logger.Warn().Err(err).Msg("story clustering: embeddings unavailable, using conservative blocking")
		return nil
	}
	if len(got) != len(items) {
		stats.EmbeddingFallback = true
		logger.Warn().Int("want", len(items)).Int("got", len(got)).
			Msg("story clustering: embedding count mismatch, using conservative blocking")
		return nil
	}
	stats.Embeddings = len(got)
	return got
}

func similarity(items []Item, a []int, b []int, embeddings [][]float64, opts Options) (float64, bool) {
	ai, bi := a[0], b[0]
	if embeddings != nil {
		score := cosine(embeddings[ai], embeddings[bi])
		return score, score >= opts.EmbeddingThreshold
	}
	score := jaccard(tokenSet(items[ai].Title+" "+excerpt(items[ai].Summary, maxSummaryForClustering)),
		tokenSet(items[bi].Title+" "+excerpt(items[bi].Summary, maxSummaryForClustering)))
	return score, score >= fallbackJaccardThreshold
}

func verify(ctx context.Context, opts Options, a, b Item, stats *Stats, logger *zerolog.Logger) (bool, bool) {
	if opts.Verifier != nil && stats.Verifications < opts.MaxVerifications {
		stats.Verifications++
		ok, err := opts.Verifier.SameDevelopment(ctx, a, b)
		if err != nil {
			logger.Warn().Err(err).Str("a", a.ID).Str("b", b.ID).
				Msg("story clustering: verification failed, leaving items separate")
			return false, true
		}
		return ok, true
	}
	stats.VerificationFallback = true
	return conservativeSame(a, b), false
}

func conservativeSame(a, b Item) bool {
	ua, ub := normalizeURL(a.Link), normalizeURL(b.Link)
	if ua != "" && ua == ub {
		return true
	}
	ta, tb := normalizeTitle(a.Title), normalizeTitle(b.Title)
	return ta != "" && ta == tb &&
		len(significantTokens(a.Title)) >= minCrossSourceTitleTokens &&
		len(significantTokens(b.Title)) >= minCrossSourceTitleTokens
}

type candidateIndex struct {
	bands map[uint64][]int
}

func newCandidateIndex() *candidateIndex {
	return &candidateIndex{bands: make(map[uint64][]int)}
}

func (c *candidateIndex) add(cluster int, items []Item, group []int) {
	seen := make(map[uint64]struct{}, len(group)*4)
	for _, idx := range group {
		for _, key := range blockKeys(items[idx]) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			c.bands[key] = append(c.bands[key], cluster)
		}
	}
}

func (c *candidateIndex) forItems(items []Item, group []int, limit int) []int {
	type bucket struct {
		key uint64
		n   int
	}
	keys := make([]bucket, 0, len(group)*16)
	seenKeys := make(map[uint64]struct{}, len(group)*16)
	for _, idx := range group {
		for _, key := range blockKeys(items[idx]) {
			if _, ok := seenKeys[key]; ok {
				continue
			}
			seenKeys[key] = struct{}{}
			keys = append(keys, bucket{key: key, n: len(c.bands[key])})
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].n != keys[j].n {
			return keys[i].n < keys[j].n
		}
		return keys[i].key < keys[j].key
	})
	seen := make(map[int]struct{}, limit)
	out := make([]int, 0, limit)
	for _, key := range keys {
		for _, cluster := range c.bands[key.key] {
			if _, ok := seen[cluster]; ok {
				continue
			}
			seen[cluster] = struct{}{}
			out = append(out, cluster)
			if len(out) == limit {
				sort.Ints(out)
				return out
			}
		}
	}
	sort.Ints(out)
	return out
}

func blockKeys(it Item) []uint64 {
	bands := simhash(significantTokens(it.Title + " " + excerpt(it.Summary, maxSummaryForClustering)))
	keys := make([]uint64, 0, 16)
	seen := make(map[uint64]struct{}, 16)
	add := func(key uint64) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for band := uint64(0); band < 4; band++ {
		add(band<<16 | (bands>>(band*16))&0xffff)
	}
	for i, token := range significantTokens(it.Title) {
		if i == 12 {
			break
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		add(uint64(1)<<63 | h.Sum64())
	}
	return keys
}

func simhash(tokens []string) uint64 {
	var bits [64]int
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		v := h.Sum64()
		for bit := 0; bit < 64; bit++ {
			if v&(uint64(1)<<uint(bit)) != 0 {
				bits[bit]++
			} else {
				bits[bit]--
			}
		}
	}
	var out uint64
	for bit := 0; bit < 64; bit++ {
		if bits[bit] > 0 {
			out |= uint64(1) << uint(bit)
		}
	}
	return out
}

func clusterID(items []ClusterItem) string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ItemID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return "story-" + hex.EncodeToString(sum[:])[:16]
}

func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimPrefix(u.Host, "www.")
	u.Fragment = ""
	u.RawFragment = ""
	q := u.Query()
	for key := range q {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "fbclid" || lower == "gclid" ||
			lower == "mc_cid" || lower == "mc_eid" || lower == "ref" || lower == "source" {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" && u.RawQuery == "" {
		return u.Scheme + "://" + u.Host
	}
	if u.RawQuery == "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	return u.Scheme + "://" + u.Host + u.Path + "?" + u.RawQuery
}

func normalizeTitle(title string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

func normalizeSource(source string) string {
	return strings.Join(strings.Fields(strings.ToLower(source)), " ")
}

func significantTokens(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len([]rune(field)) < 3 {
			continue
		}
		if _, stop := stopWords[field]; stop {
			continue
		}
		out = append(out, field)
	}
	return out
}

func tokenSet(text string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, token := range significantTokens(text) {
		out[token] = struct{}{}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, aa, bb float64
	for i := range a {
		dot += a[i] * b[i]
		aa += a[i] * a[i]
		bb += b[i] * b[i]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / (math.Sqrt(aa) * math.Sqrt(bb))
}

func excerpt(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= limit {
		return string(r)
	}
	return string(r[:limit])
}

var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "from": {}, "that": {}, "this": {},
	"are": {}, "was": {}, "were": {}, "has": {}, "have": {}, "had": {}, "its": {},
	"into": {}, "over": {}, "under": {}, "after": {}, "before": {}, "about": {},
	"how": {}, "why": {}, "what": {}, "when": {}, "where": {}, "who": {}, "will": {},
	"can": {}, "could": {}, "should": {}, "would": {}, "not": {}, "but": {}, "you": {},
	"your": {}, "their": {}, "they": {}, "them": {}, "his": {}, "her": {}, "our": {},
	"more": {}, "most": {}, "new": {}, "says": {}, "said": {}, "one": {}, "two": {},
}
