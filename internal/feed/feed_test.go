package feed

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"networkai/internal/search"
)

type fakeSignals struct {
	liked    []string
	counts   map[string]int
	affinity map[string]int
}

func (f fakeSignals) RecentLikedPostIDs(context.Context, int64, int) ([]string, error) {
	return f.liked, nil
}
func (f fakeSignals) LikeCounts(context.Context, []string) (map[string]int, error) {
	return f.counts, nil
}
func (f fakeSignals) AuthorAffinity(context.Context, int64) (map[string]int, error) {
	return f.affinity, nil
}

// fakeIndex is called from parallel goroutines (one per retrieval source),
// so it guards its own state, exactly like a real client must.
type fakeIndex struct {
	mu      sync.Mutex
	embs    map[string][]float32
	bySrc   map[string][]search.Doc
	queries []search.CandidateQuery
}

func (f *fakeIndex) Embeddings(_ context.Context, ids []string) (map[string][]float32, error) {
	out := map[string][]float32{}
	for _, id := range ids {
		if v, ok := f.embs[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (f *fakeIndex) Candidates(_ context.Context, q search.CandidateQuery) ([]search.Doc, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	switch {
	case q.Vector != nil:
		return f.bySrc["semantic"], nil
	case q.LikeIDs != nil:
		return f.bySrc["lexical"], nil
	default:
		return f.bySrc["fresh"], nil
	}
}

var now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func ranker() *Ranker {
	r := &Ranker{
		Features: []string{"cosine", "log_likes", "freshness", "author_affinity", "has_history"},
		Mean:     []float64{0, 0, 0, 0, 0}, Std: []float64{1, 1, 1, 1, 1},
		Weights: []float64{2, 0.5, 1, 0.1, 0},
	}
	r.index = map[string]int{}
	for i, f := range r.Features {
		r.index[f] = i
	}
	return r
}

func doc(id string, vec []float32, ageH int) search.Doc {
	return search.Doc{ID: id, Author: "a_" + id, Embedding: vec, CreatedAt: now.Add(-time.Duration(ageH) * time.Hour)}
}

func TestForYouMergesSourcesAndRanks(t *testing.T) {
	coffee, hiking := []float32{1, 0}, []float32{0, 1}
	ix := &fakeIndex{
		embs: map[string][]float32{"liked1": coffee},
		bySrc: map[string][]search.Doc{
			"semantic": {doc("c1", coffee, 10), doc("c2", coffee, 30)},
			"lexical":  {doc("c1", coffee, 10)},
			"fresh":    {doc("h1", hiking, 1), doc("c1", coffee, 10)},
		},
	}
	s := &Service{Signals: fakeSignals{liked: []string{"liked1"}}, Index: ix, Ranker: ranker(), Now: func() time.Time { return now }}
	items, err := s.ForYou(context.Background(), 1, "me", 10)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, it := range items {
		order = append(order, it.Doc.ID)
	}
	// c1 is on-topic and fresh-ish; h1 is freshest but off-topic.
	if strings.Join(order, ",") != "c1,c2,h1" && strings.Join(order, ",") != "c1,h1,c2" {
		t.Fatalf("order = %v", order)
	}
	if items[0].Doc.ID != "c1" || len(items[0].Sources) != 3 {
		t.Fatalf("c1 should rank first and be found by all 3 sources: %+v", items[0])
	}
	for _, q := range ix.queries {
		if q.ExcludeAuthor != "me" || len(q.ExcludeIDs) != 1 || q.ExcludeIDs[0] != "liked1" {
			t.Fatalf("every source must exclude own and already-liked posts: %+v", q)
		}
	}
}

// A user with no likes has no taste vector: only the fresh source runs and
// the ranker falls back to freshness and engagement.
func TestColdStartUsesFreshOnly(t *testing.T) {
	ix := &fakeIndex{bySrc: map[string][]search.Doc{
		"fresh": {doc("old", nil, 40), doc("new", nil, 1), doc("popular", nil, 20)},
	}}
	s := &Service{Signals: fakeSignals{counts: map[string]int{"popular": 1000}}, Index: ix, Ranker: ranker(),
		Now: func() time.Time { return now }}
	items, err := s.ForYou(context.Background(), 1, "me", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.queries) != 1 || ix.queries[0].Vector != nil || ix.queries[0].LikeIDs != nil {
		t.Fatalf("cold start should only query the fresh source: %+v", ix.queries)
	}
	if len(items) != 2 || items[0].Features["cosine"] != 0 {
		t.Fatalf("items = %+v", items)
	}
}

func TestDiversifyCapsPostsPerAuthor(t *testing.T) {
	mk := func(id, author string) Item { return Item{Doc: search.Doc{ID: id, Author: author}} }
	in := []Item{mk("1", "spam"), mk("2", "spam"), mk("3", "spam"), mk("4", "bob"), mk("5", "spam"), mk("6", "cat")}
	var got []string
	for _, it := range Diversify(in, 2) {
		got = append(got, it.Doc.ID)
	}
	if strings.Join(got, ",") != "1,2,4,6,3,5" {
		t.Fatalf("order = %v, want 1,2,4,6,3,5", got)
	}
}

func TestFeaturesMatchTrainingDefinitions(t *testing.T) {
	f := Features([]float32{1, 0}, doc("x", []float32{1, 0}, 24), 3, 1, 1, now)
	want := map[string]float64{"cosine": 1, "log_likes": 1.3862943611198906, "freshness": 0.36787944117144233,
		"author_affinity": 0.6931471805599453, "has_history": 1}
	for k, v := range want {
		if d := f[k] - v; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s = %v, want %v", k, f[k], v)
		}
	}
}

func TestLoadRankerFromTraining(t *testing.T) {
	r, err := LoadRanker("../../models/ranker.json")
	if err != nil {
		t.Fatal(err)
	}
	fresh := r.Score(map[string]float64{"freshness": 1, "cosine": 0.5})
	stale := r.Score(map[string]float64{"freshness": 0.05, "cosine": 0.5})
	if fresh <= stale {
		t.Fatal("trained ranker should prefer fresher posts, all else equal")
	}
}
