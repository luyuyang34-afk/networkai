// Package feed builds the personalised "For you" feed in two stages.
//
// Stage 1, candidate retrieval (recall: find a few hundred plausible posts
// out of everything recent, cheaply), from three sources:
//   - semantic: kNN on post embeddings around the user's taste vector (the
//     mean embedding of posts they liked recently)
//   - lexical: Elasticsearch more_like_this on the posts they liked
//   - fresh: newest posts, so new users and new topics still get exposure
//
// Stage 2, ranking (precision: order the candidates well): a logistic
// regression over freshness, engagement, embedding similarity and author
// affinity. It is trained offline on past interactions
// (tools/ranking/train_eval.py) and loaded from models/ranker.json. The
// feature definitions here must match the training script's.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"networkai/internal/embed"
	"networkai/internal/search"
)

// Ranker is a logistic-regression scorer over standardised features.
type Ranker struct {
	Features []string  `json:"features"`
	Mean     []float64 `json:"mean"`
	Std      []float64 `json:"std"`
	Weights  []float64 `json:"weights"`
	Bias     float64   `json:"bias"`
	index    map[string]int
}

func LoadRanker(path string) (*Ranker, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Ranker
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	n := len(r.Features)
	if n == 0 || len(r.Mean) != n || len(r.Std) != n || len(r.Weights) != n {
		return nil, fmt.Errorf("%s: inconsistent ranker shapes", path)
	}
	r.index = map[string]int{}
	for i, f := range r.Features {
		r.index[f] = i
	}
	return &r, nil
}

// Score returns the ranking score (log-odds of a like). Features the model
// does not know are ignored; a missing feature contributes zero (as if it
// were at its training mean).
func (r *Ranker) Score(f map[string]float64) float64 {
	s := r.Bias
	for name, v := range f {
		if i, ok := r.index[name]; ok {
			s += r.Weights[i] * (v - r.Mean[i]) / r.Std[i]
		}
	}
	return s
}

// ---------- dependencies ----------

type Signals interface {
	// RecentLikedPostIDs returns ids of posts the user liked, newest first.
	RecentLikedPostIDs(ctx context.Context, userID int64, limit int) ([]string, error)
	// LikeCounts returns like counts for the given post ids.
	LikeCounts(ctx context.Context, postIDs []string) (map[string]int, error)
	// AuthorAffinity returns, per author username, how many of their posts
	// the user has liked.
	AuthorAffinity(ctx context.Context, userID int64) (map[string]int, error)
}

type Index interface {
	Embeddings(ctx context.Context, ids []string) (map[string][]float32, error)
	Candidates(ctx context.Context, q search.CandidateQuery) ([]search.Doc, error)
	Docs(ctx context.Context, ids []string) (map[string]search.Doc, error)
}

type Service struct {
	Signals Signals
	Index   Index
	Ranker  *Ranker
	// Window limits candidates to recent posts (default 72h).
	Window time.Duration
	Now    func() time.Time

	// Bulkhead: at most MaxConcurrent feed requests run at once (0 = no
	// limit). Each one issues three heavy Elasticsearch searches, and without
	// a cap they fill Elasticsearch's search thread pool and starve plain
	// /posts/search (measured: 429 rejections and multi-second latencies).
	// Excess requests wait up to QueueWait, then fail fast with ErrBusy.
	MaxConcurrent int
	QueueWait     time.Duration

	// SemanticK is how many kNN candidates to retrieve (default 100).
	SemanticK int
	// Lexical enables the more_like_this source. Off by default: in a sample
	// of 1,200 served items only 1% came from it alone, while it was the most
	// expensive query (see docs/benchmark.md).
	Lexical bool
	semOnce sync.Once
	sem     chan struct{}
}

// ErrBusy means the feed shed this request to protect the rest of the system.
var ErrBusy = errors.New("feed at capacity")

func (s *Service) acquire(ctx context.Context) (func(), error) {
	if s.MaxConcurrent <= 0 {
		return func() {}, nil
	}
	s.semOnce.Do(func() { s.sem = make(chan struct{}, s.MaxConcurrent) })
	wait := s.QueueWait
	if wait <= 0 {
		wait = 100 * time.Millisecond
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type Item struct {
	Doc      search.Doc         `json:"-"`
	Score    float64            `json:"score"`
	Features map[string]float64 `json:"features"`
	Sources  []string           `json:"sources"`
}

// ForYou returns up to limit ranked posts for the user.
func (s *Service) ForYou(ctx context.Context, userID int64, username string, limit int) ([]Item, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	window := s.Window
	if window <= 0 {
		window = 72 * time.Hour
	}

	liked, err := s.Signals.RecentLikedPostIDs(ctx, userID, 50)
	if err != nil {
		return nil, err
	}
	var taste []float32
	if len(liked) > 0 {
		embs, err := s.Index.Embeddings(ctx, liked)
		if err != nil {
			return nil, err
		}
		vecs := make([][]float32, 0, len(embs))
		for _, v := range embs {
			vecs = append(vecs, v)
		}
		taste = embed.Mean(vecs)
	}

	// ---- stage 1: candidate retrieval, sources in parallel
	// Lexical and fresh candidates need their vectors for the cosine feature
	// (when the user has a taste vector at all); kNN candidates do not,
	// because their score already encodes the cosine.
	base := search.CandidateQuery{Since: now.Add(-window), ExcludeIDs: liked, ExcludeAuthor: username,
		IncludeEmbedding: taste != nil}
	type source struct {
		name string
		q    search.CandidateQuery
		docs []search.Doc
	}
	srcs := []*source{{name: "fresh", q: sized(base, 100)}}
	k := s.SemanticK
	if k <= 0 {
		k = 100
	}
	if taste != nil {
		q := sized(base, k)
		q.Vector = taste
		q.IncludeEmbedding = false
		srcs = append(srcs, &source{name: "semantic", q: q})
	}
	if s.Lexical && len(liked) > 0 {
		q := sized(base, 100)
		q.LikeIDs = liked[:min(len(liked), 20)]
		srcs = append(srcs, &source{name: "lexical", q: q})
	}
	// Sources run one after another, not in parallel. Parallel retrieval
	// was ~15 ms faster per request, but each feed request then held up to
	// three Elasticsearch search threads at once. Under load that starved
	// plain search (thread-pool rejections). Sequential retrieval plus the
	// MaxConcurrent bulkhead bounds the feed to MaxConcurrent heavy queries
	// in Elasticsearch at any moment. See docs/benchmark.md.
	for _, src := range srcs {
		docs, err := s.Index.Candidates(ctx, src.q)
		if err != nil {
			return nil, fmt.Errorf("%s candidates: %w", src.name, err)
		}
		src.docs = docs
	}

	byID := map[string]*Item{}
	cosine := map[string]float64{}
	var ids []string
	for _, src := range srcs {
		for _, d := range src.docs {
			it, ok := byID[d.ID]
			if !ok {
				it = &Item{Doc: d}
				byID[d.ID] = it
				ids = append(ids, d.ID)
			}
			it.Sources = append(it.Sources, src.name)
			if src.name == "semantic" {
				cosine[d.ID] = 2*d.Score - 1 // ES cosine score is (1+cos)/2
			} else if _, known := cosine[d.ID]; !known && d.Embedding != nil {
				cosine[d.ID] = embed.Cosine(taste, d.Embedding)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// ---- stage 2: features and ranking
	counts, err := s.Signals.LikeCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	affinity, err := s.Signals.AuthorAffinity(ctx, userID)
	if err != nil {
		return nil, err
	}
	hasHistory := 0.0
	if taste != nil {
		hasHistory = 1
	}
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		it := byID[id]
		it.Features = Features(cosine[id], it.Doc, counts[id], affinity[it.Doc.Author], hasHistory, now)
		it.Score = s.Ranker.Score(it.Features)
		items = append(items, *it)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].Doc.CreatedAt.After(items[j].Doc.CreatedAt)
	})
	items = Diversify(items, MaxPerAuthor)
	items = items[:min(limit, len(items))]

	// Hydrate only the final page with full text (candidates carried just
	// the fields needed for ranking).
	pageIDs := make([]string, len(items))
	for i, it := range items {
		pageIDs[i] = it.Doc.ID
	}
	full, err := s.Index.Docs(ctx, pageIDs)
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, it := range items {
		if d, ok := full[it.Doc.ID]; ok { // skip posts deleted since retrieval
			it.Doc = d
			out = append(out, it)
		}
	}
	return out, nil
}

// MaxPerAuthor caps how many posts by one author appear before every other
// candidate has been shown.
const MaxPerAuthor = 2

// Diversify keeps score order but defers an author's posts beyond the first
// max, so one prolific (or spammy) author cannot fill the feed. Deferred
// posts keep their relative order and follow everything else.
func Diversify(items []Item, max int) []Item {
	seen := map[string]int{}
	out := make([]Item, 0, len(items))
	var deferred []Item
	for _, it := range items {
		if seen[it.Doc.Author] < max {
			seen[it.Doc.Author]++
			out = append(out, it)
		} else {
			deferred = append(deferred, it)
		}
	}
	return append(out, deferred...)
}

// Features computes the ranking features for one candidate. Definitions match
// build_examples() in tools/ranking/train_eval.py.
func Features(cosine float64, d search.Doc, likes, affinity int, hasHistory float64, now time.Time) map[string]float64 {
	age := now.Sub(d.CreatedAt).Hours()
	if age < 0 {
		age = 0
	}
	return map[string]float64{
		"cosine":          cosine,
		"log_likes":       math.Log1p(float64(likes)),
		"freshness":       math.Exp(-age / 24),
		"author_affinity": math.Log1p(float64(affinity)),
		"has_history":     hasHistory,
	}
}

func sized(q search.CandidateQuery, n int) search.CandidateQuery {
	q.Size = n
	return q
}
