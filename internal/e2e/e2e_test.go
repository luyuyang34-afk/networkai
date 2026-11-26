// Package e2e exercises the whole write path over HTTP against real
// PostgreSQL and Elasticsearch:
//
//	HTTP API -> PostgreSQL (post + outbox, one transaction)
//	         -> outbox worker -> indexer -> Elasticsearch -> /posts/search
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"networkai/internal/auth"
	"networkai/internal/embed"
	"networkai/internal/feed"
	"networkai/internal/httpapi"
	"networkai/internal/indexer"
	"networkai/internal/media"
	"networkai/internal/outbox"
	"networkai/internal/search"
	"networkai/internal/store"
	"networkai/internal/testutil"
	"networkai/internal/timeline"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func init() { auth.Cost = bcrypt.MinCost }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	api    *httptest.Server
	es     *search.Client
	worker *outbox.Worker
	ix     *indexer.Indexer
}

type readiness struct {
	*store.Store
	pool *pgxpool.Pool
}

func (r readiness) OutboxStats(ctx context.Context) (outbox.Stats, error) {
	return outbox.ReadStats(ctx, r.pool)
}

func setup(t *testing.T) *env {
	t.Helper()
	esURL := testutil.ESURL(t)
	pool := testutil.NewDB(t)
	st := store.New(pool)

	alias := fmt.Sprintf("posts_test_%d", rand.Uint64())
	es := search.New(esURL, "", "").WithAlias(alias)
	if err := es.EnsureIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, esURL+"/"+alias+"_v1", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})

	ms, err := media.NewLocal(t.TempDir(), "http://media.test")
	if err != nil {
		t.Fatal(err)
	}
	lsa, err := embed.Load("../../models/lsa.json")
	if err != nil {
		t.Fatal(err)
	}
	ranker, err := feed.LoadRanker("../../models/ranker.json")
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(httpapi.New(httpapi.Deps{
		Users: st, Posts: st, Search: es, Media: ms, Likes: st,
		Feed:   &feed.Service{Signals: st, Index: es, Ranker: ranker, Lexical: true},
		Ready:  readiness{Store: st, pool: pool},
		Tokens: auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour),
		Log:    quiet,
	}))
	t.Cleanup(api.Close)

	ix := &indexer.Indexer{Posts: st, Index: es, Embed: lsa.Embed}
	w := &outbox.Worker{Pool: pool, Handle: ix.Handle, Log: quiet,
		Backoff: func(int) time.Duration { return 0 }, MaxAttempts: 3,
		IsUnavailable: func(err error) bool { return errors.Is(err, search.ErrUnavailable) }}
	return &env{t: t, pool: pool, api: api, es: es, worker: w, ix: ix}
}

func (e *env) do(method, path, token string, body io.Reader, contentType string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.api.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck // 204 has no body
	return resp.StatusCode, out
}

func (e *env) jsonReq(method, path, token string, v any) (int, map[string]any) {
	b, _ := json.Marshal(v)
	return e.do(method, path, token, bytes.NewReader(b), "application/json")
}

func (e *env) login(user string) string {
	e.t.Helper()
	creds := map[string]string{"username": user, "password": "password123"}
	if code, body := e.jsonReq("POST", "/users", "", creds); code != http.StatusCreated {
		e.t.Fatalf("signup %s: %d %v", user, code, body)
	}
	code, body := e.jsonReq("POST", "/auth/login", "", creds)
	if code != http.StatusOK {
		e.t.Fatalf("login %s: %d %v", user, code, body)
	}
	return body["token"].(string)
}

func (e *env) createPost(token, message string) string {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("message", message) //nolint:errcheck
	mw.Close()
	code, body := e.do("POST", "/posts", token, &buf, mw.FormDataContentType())
	if code != http.StatusCreated {
		e.t.Fatalf("create post: %d %v", code, body)
	}
	return body["id"].(string)
}

// drain runs the worker until the outbox is empty.
func (e *env) drain() {
	e.t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := e.worker.ProcessBatch(context.Background()); err != nil {
			e.t.Fatal(err)
		}
		if s := e.stats(); s.Pending == 0 {
			if err := e.es.Refresh(context.Background()); err != nil {
				e.t.Fatal(err)
			}
			return
		}
	}
	e.t.Fatalf("outbox did not drain: %+v", e.stats())
}

func (e *env) stats() outbox.Stats {
	s, err := outbox.ReadStats(context.Background(), e.pool)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) searchIDs(query string) []string {
	e.t.Helper()
	code, body := e.do("GET", "/posts/search?q="+query, "", nil, "")
	if code != http.StatusOK {
		e.t.Fatalf("search %q: %d %v", query, code, body)
	}
	var ids []string
	for _, it := range body["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

func TestCreateEditDeleteIsReflectedInSearch(t *testing.T) {
	e := setup(t)
	alice := e.login("alice")

	id := e.createPost(alice, "Running along the river at sunrise")
	e.drain()

	// Stemming: "runs" matches "Running".
	if ids := e.searchIDs("runs"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("search 'runs' = %v, want [%s]", ids, id)
	}

	if code, body := e.jsonReq("PATCH", "/posts/"+id, alice, map[string]string{"message": "Cycling in the mountains"}); code != 200 {
		t.Fatalf("edit: %d %v", code, body)
	}
	e.drain()
	if ids := e.searchIDs("river"); len(ids) != 0 {
		t.Fatalf("old text still searchable after edit: %v", ids)
	}
	if ids := e.searchIDs("mountain"); len(ids) != 1 {
		t.Fatalf("new text not searchable after edit: %v", ids)
	}

	if code, _ := e.do("DELETE", "/posts/"+id, alice, nil, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	e.drain()
	if ids := e.searchIDs("mountain"); len(ids) != 0 {
		t.Fatalf("deleted post still searchable: %v", ids)
	}
}

// The headline reliability property. With Elasticsearch unreachable:
//   - posting, editing and browsing keep working (PostgreSQL is the source of truth)
//   - search returns a clean 503, not a 500
//   - nothing is lost: when Elasticsearch comes back the backlog drains and
//     search reflects every change, including edits and deletes made during
//     the outage.
func TestElasticsearchOutageLosesNoWrites(t *testing.T) {
	e := setup(t)
	alice := e.login("alice")

	// Simulate the outage: the indexer and search both point at a closed port.
	dead := search.New("http://127.0.0.1:1", "", "")
	e.ix.Index = dead
	deadAPI := httptest.NewServer(httpapi.New(httpapi.Deps{
		Users: store.New(e.pool), Posts: store.New(e.pool), Search: dead,
		Media: mustLocal(t), Tokens: auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour), Log: quiet,
	}))
	defer deadAPI.Close()
	liveAPI := e.api
	e.api = deadAPI

	const n = 20
	ids := make([]string, n)
	for i := range ids {
		ids[i] = e.createPost(alice, fmt.Sprintf("outage post number %d about kayaking", i))
	}
	e.jsonReq("PATCH", "/posts/"+ids[0], alice, map[string]string{"message": "edited during outage: hiking"})
	if code, _ := e.do("DELETE", "/posts/"+ids[1], alice, nil, ""); code != http.StatusNoContent {
		t.Fatalf("delete during outage: %d", code)
	}

	// Browsing comes from PostgreSQL and still works.
	if code, body := e.do("GET", "/posts?limit=100", "", nil, ""); code != 200 || len(body["items"].([]any)) != n-1 {
		t.Fatalf("browse during outage: %d, %d items", code, len(body["items"].([]any)))
	}
	// Search degrades gracefully.
	if code, body := e.do("GET", "/posts/search?q=kayaking", "", nil, ""); code != http.StatusServiceUnavailable ||
		body["error"].(map[string]any)["code"] != "search_unavailable" {
		t.Fatalf("search during outage: %d %v", code, body)
	}

	// The worker keeps probing. MaxAttempts is only 3, yet after many probes
	// nothing is dead-lettered: an unavailable dependency pauses the worker
	// instead of spending each event's retry budget.
	for i := 0; i < 10; i++ {
		if _, err := e.worker.ProcessBatch(context.Background()); !errors.Is(err, outbox.ErrPaused) {
			t.Fatalf("probe %d during outage: err = %v, want ErrPaused", i, err)
		}
	}
	if s := e.stats(); s.Pending != n+2 || s.Dead != 0 {
		t.Fatalf("during outage: stats %+v, want %d pending, 0 dead", s, n+2)
	}

	// Elasticsearch recovers.
	e.ix.Index = e.es
	e.api = liveAPI
	e.drain()

	got := e.searchIDs("kayaking")
	if len(got) != n-2 { // minus the edited one and the deleted one
		t.Fatalf("after recovery: %d results for 'kayaking', want %d", len(got), n-2)
	}
	for _, id := range got {
		if id == ids[0] || id == ids[1] {
			t.Fatalf("stale post %s in results", id)
		}
	}
	if hits := e.searchIDs("hiking"); len(hits) != 1 || hits[0] != ids[0] {
		t.Fatalf("edit made during outage not indexed: %v", hits)
	}
}

// External versioning makes stale and duplicate index writes harmless no
// matter what order they arrive in.
func TestOutOfOrderIndexWritesConverge(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	doc := search.Doc{ID: "p1", Author: "alice", MediaType: "none", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	v3 := doc
	v3.Message, v3.Deleted = "final", true
	v2 := doc
	v2.Message = "second"
	v1 := doc
	v1.Message = "first"

	// Deliver newest first, then the stale ones, then a duplicate.
	for _, w := range []struct {
		d    search.Doc
		v    int64
		want bool
	}{{v3, 3, true}, {v1, 1, false}, {v2, 2, false}, {v3, 3, false}} {
		applied, err := e.es.Upsert(ctx, w.d, w.v)
		if err != nil {
			t.Fatal(err)
		}
		if applied != w.want {
			t.Fatalf("version %d applied=%v, want %v", w.v, applied, w.want)
		}
	}
	e.es.Refresh(ctx) //nolint:errcheck
	res, err := e.es.Search(ctx, search.Query{Text: "second", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Docs) != 0 {
		t.Fatal("stale version resurrected a deleted post")
	}
}

func TestSearchPaginationAndFilters(t *testing.T) {
	e := setup(t)
	alice, bob := e.login("alice"), e.login("bob")
	for i := 0; i < 7; i++ {
		e.createPost(alice, fmt.Sprintf("coffee review %d", i))
	}
	for i := 0; i < 3; i++ {
		e.createPost(bob, fmt.Sprintf("coffee notes %d", i))
	}
	e.drain()

	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		path := "/posts/search?q=coffee&limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		code, body := e.do("GET", path, "", nil, "")
		if code != 200 {
			t.Fatalf("page %d: %d %v", page, code, body)
		}
		for _, it := range body["items"].([]any) {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s across pages", id)
			}
			seen[id] = true
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 10 {
		t.Fatalf("paged through %d results, want 10", len(seen))
	}

	code, body := e.do("GET", "/posts/search?q=coffee&author=bob", "", nil, "")
	if code != 200 || len(body["items"].([]any)) != 3 {
		t.Fatalf("author filter: %d, %v", code, body)
	}
	if code, _ := e.do("GET", "/posts/search?cursor=garbage", "", nil, ""); code != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d, want 400", code)
	}
}

func mustLocal(t *testing.T) media.Store {
	m, err := media.NewLocal(t.TempDir(), "http://media.test")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPurgeTombstonesRemovesOnlyOldDeletedDocs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old := time.Now().Add(-30 * 24 * time.Hour)
	docs := []struct {
		id      string
		deleted bool
		updated time.Time
	}{
		{"old-tombstone", true, old},
		{"new-tombstone", true, time.Now()},
		{"old-live", false, old},
	}
	for _, d := range docs {
		if _, err := e.es.Upsert(ctx, search.Doc{ID: d.id, Author: "a", Message: "purge test", MediaType: "none",
			CreatedAt: d.updated, UpdatedAt: d.updated, Deleted: d.deleted}, 5); err != nil {
			t.Fatal(err)
		}
	}
	e.es.Refresh(ctx) //nolint:errcheck
	n, err := e.es.PurgeTombstones(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1 (only the old tombstone)", n)
	}
	// The purged post can't come back: a late event re-reads PostgreSQL
	// (which says deleted) and writes a new tombstone, still filtered out.
	if res, _ := e.es.Search(ctx, search.Query{Text: "purge", Limit: 10}); len(res.Docs) != 1 || res.Docs[0].ID != "old-live" {
		t.Fatalf("after purge, search = %+v", res.Docs)
	}
}

// Reindex must rebuild exactly what PostgreSQL says (including deletes and
// edits) across several bulk batches, and must never overwrite a newer
// version that the live worker already indexed.
func TestBulkReindexMatchesPostgres(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.login("alice") // creates the author
	// 1201 posts = 3 bulk batches, the last one partial.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO posts (id, author_id, message, media_type)
		SELECT gen_random_uuid(), (SELECT id FROM users WHERE username = 'alice'),
		       'bulk post ' || g || CASE WHEN g % 100 = 0 THEN ' special' ELSE '' END, 'none'
		FROM generate_series(1, 1201) g`); err != nil {
		t.Fatal(err)
	}
	// Soft-delete 3 of the 12 'special' posts and edit another.
	if _, err := e.pool.Exec(ctx, `
		UPDATE posts SET deleted_at = now(), version = version + 1
		WHERE id IN (SELECT id FROM posts WHERE message LIKE '%special' ORDER BY id LIMIT 3)`); err != nil {
		t.Fatal(err)
	}
	var edited string
	if err := e.pool.QueryRow(ctx, `
		UPDATE posts SET message = 'rewritten text', version = version + 1
		WHERE id = (SELECT id FROM posts WHERE message LIKE '%special' AND deleted_at IS NULL ORDER BY id LIMIT 1)
		RETURNING id::text`).Scan(&edited); err != nil {
		t.Fatal(err)
	}
	// The live worker already indexed the edited post at a *newer* version
	// than PostgreSQL will hand the reindex. Reindex must not clobber it.
	if _, err := e.es.Upsert(ctx, search.Doc{ID: edited, Author: "alice", Message: "newest from live worker",
		MediaType: "none", CreatedAt: time.Now(), UpdatedAt: time.Now()}, 99); err != nil {
		t.Fatal(err)
	}

	n, err := e.ix.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1201 {
		t.Fatalf("reindexed %d posts, want 1201", n)
	}
	e.es.Refresh(ctx) //nolint:errcheck

	res, err := e.es.Search(ctx, search.Query{Text: "special", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Docs) != 12-3-1 {
		t.Fatalf("'special' hits = %d, want 8 (12 minus 3 deleted minus 1 edited)", len(res.Docs))
	}
	res, _ = e.es.Search(ctx, search.Query{Text: "newest live worker", Limit: 5})
	if len(res.Docs) != 1 || res.Docs[0].ID != edited {
		t.Fatalf("reindex overwrote a newer version: %+v", res.Docs)
	}
}

func TestLikesAreIdempotentAndCounted(t *testing.T) {
	e := setup(t)
	alice, bob := e.login("alice"), e.login("bob")
	id := e.createPost(alice, "a post worth liking")

	for i := 0; i < 2; i++ { // liking twice is a no-op
		if code, _ := e.do("POST", "/posts/"+id+"/like", bob, nil, ""); code != http.StatusNoContent {
			t.Fatalf("like: %d", code)
		}
	}
	_, body := e.do("GET", "/posts/"+id, bob, nil, "")
	if body["like_count"].(float64) != 1 || body["liked"] != true {
		t.Fatalf("as bob: %v", body)
	}
	_, body = e.do("GET", "/posts/"+id, "", nil, "")
	if body["like_count"].(float64) != 1 || body["liked"] != false {
		t.Fatalf("anonymous: %v", body)
	}
	if code, _ := e.do("DELETE", "/posts/"+id+"/like", bob, nil, ""); code != http.StatusNoContent {
		t.Fatalf("unlike: %d", code)
	}
	if _, body = e.do("GET", "/posts/"+id, bob, nil, ""); body["like_count"].(float64) != 0 {
		t.Fatalf("after unlike: %v", body)
	}
	if code, _ := e.do("POST", "/posts/"+uuidString()+"/like", bob, nil, ""); code != http.StatusNotFound {
		t.Fatalf("like missing post: %d", code)
	}
}

// End to end: after liking coffee posts, a user's For-you feed should put
// unseen coffee posts above unrelated ones, and never show their own or
// already-liked posts.
func TestForYouFeedReflectsLikes(t *testing.T) {
	e := setup(t)
	alice, bob, carol := e.login("alice"), e.login("bob"), e.login("carol")

	var liked []string
	for _, m := range []string{"espresso roast with crema at the cafe", "latte art and arabica beans",
		"pourover coffee brew with a new grinder"} {
		liked = append(liked, e.createPost(bob, m))
	}
	unseenCoffee := e.createPost(carol, "barista notes: espresso, cappuccino and a fresh roast")
	for _, m := range []string{"summit hike on the forest trail", "deadlift and squat workout at the gym",
		"new indie game boss fight speedrun"} {
		e.createPost(carol, m)
	}
	e.createPost(alice, "my own espresso post") // alice must not see her own posts
	e.drain()

	for _, id := range liked {
		if code, _ := e.do("POST", "/posts/"+id+"/like", alice, nil, ""); code != http.StatusNoContent {
			t.Fatalf("like: %d", code)
		}
	}
	code, body := e.do("GET", "/feed?limit=10", alice, nil, "")
	if code != 200 {
		t.Fatalf("feed: %d %v", code, body)
	}
	items := body["items"].([]any)
	if len(items) == 0 {
		t.Fatal("empty feed")
	}
	first := items[0].(map[string]any)
	if first["id"] != unseenCoffee {
		t.Fatalf("first item = %v (%v), want the unseen coffee post", first["message"], first["ranking"])
	}
	for _, it := range items {
		m := it.(map[string]any)
		if m["author"] == "alice" {
			t.Fatal("own post in feed")
		}
		for _, id := range liked {
			if m["id"] == id {
				t.Fatal("already-liked post in feed")
			}
		}
	}
	srcs := first["ranking"].(map[string]any)["sources"].([]any)
	if len(srcs) < 2 {
		t.Fatalf("expected the coffee post to be retrieved by several sources, got %v", srcs)
	}
}

func uuidString() string { return "6f1c8b7e-0000-4000-8000-000000000000" }

// swapHook simulates the live worker writing to the OLD index between the
// bulk load and the alias swap, the window a blue/green rebuild must cover.
type swapHook struct {
	*search.Client
	beforeSwap func()
}

func (h swapHook) SwapAlias(ctx context.Context, idx string) ([]string, error) {
	h.beforeSwap()
	return h.Client.SwapAlias(ctx, idx)
}

func TestRebuildFreshSwapsAliasAndCatchesUp(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.login("alice")
	keep := e.createPost(alice, "espresso before the rebuild")
	edited := e.createPost(alice, "this text will change during the rebuild")
	e.drain()

	// Simulate an index built before embeddings existed.
	noEmbed := &indexer.Indexer{Posts: e.ix.Posts, Index: e.es}
	if _, err := noEmbed.Reindex(ctx); err != nil {
		t.Fatal(err)
	}

	hook := swapHook{Client: e.es, beforeSwap: func() {
		// A user edits a post mid-rebuild; the worker writes it to the old index.
		if code, _ := e.jsonReq("PATCH", "/posts/"+edited, alice, map[string]string{"message": "changed mid rebuild"}); code != 200 {
			t.Fatalf("edit: %d", code)
		}
		e.drain()
	}}
	newIdx := fmt.Sprintf("%s_fresh", e.es.Alias())
	loaded, caught, err := e.ix.RebuildFresh(ctx, hook, newIdx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 2 || caught < 1 {
		t.Fatalf("loaded=%d caught_up=%d", loaded, caught)
	}
	e.es.Refresh(ctx) //nolint:errcheck

	embs, err := e.es.Embeddings(ctx, []string{keep})
	if err != nil || len(embs[keep]) != search.EmbeddingDims {
		t.Fatalf("documents in the new index should carry embeddings: %v %v", embs, err)
	}
	if hits := e.searchIDs("mid+rebuild"); len(hits) != 1 || hits[0] != edited {
		t.Fatalf("edit made during the rebuild was lost: %v", hits)
	}
	resp, err := http.Get(testutil.ESURL(t) + "/" + e.es.Alias() + "_v1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old index should be deleted, got %d", resp.StatusCode)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, testutil.ESURL(t)+"/"+newIdx, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	})
}

// Over HTTP: follow, post, read the home timeline; unfollow drops the
// followee's posts immediately (the cached timeline is invalidated).
func TestHomeTimelineOverHTTP(t *testing.T) {
	e := setup(t)
	rdb, prefix := testutil.Redis(t)
	st := store.New(e.pool)
	tl := &timeline.Service{Redis: rdb, Store: st, CelebrityThreshold: 1000, Prefix: prefix}
	api := httptest.NewServer(httpapi.New(httpapi.Deps{
		Users: st, Posts: st, Search: e.es, Media: mustLocal(t), Social: st, Timelines: tl,
		Tokens: auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour), Log: quiet,
	}))
	defer api.Close()
	e.api = api
	alice, bob := e.login("alice"), e.login("bob")

	if code, _ := e.do("POST", "/users/bob/follow", alice, nil, ""); code != http.StatusNoContent {
		t.Fatalf("follow: %d", code)
	}
	if code, body := e.do("GET", "/users/bob", alice, nil, ""); code != 200 || body["follower_count"].(float64) != 1 || body["followed_by_me"] != true {
		t.Fatalf("profile: %d %v", code, body)
	}
	id := e.createPost(bob, "hello followers")
	ev := outbox.Event{AggregateType: store.AggregatePost, AggregateID: id}
	if err := tl.Handle(context.Background(), ev); err != nil { // what the worker does
		t.Fatal(err)
	}
	code, body := e.do("GET", "/timeline", alice, nil, "")
	if code != 200 || len(body["items"].([]any)) != 1 {
		t.Fatalf("timeline: %d %v", code, body)
	}
	e.do("DELETE", "/users/bob/follow", alice, nil, "")
	if _, body := e.do("GET", "/timeline", alice, nil, ""); len(body["items"].([]any)) != 0 {
		t.Fatalf("after unfollow: %v", body)
	}
	if code, _ := e.do("POST", "/users/alice/follow", alice, nil, ""); code != http.StatusBadRequest {
		t.Fatalf("self-follow: %d", code)
	}
}

// Over HTTP: a write returns X-Consistency-Token; sending it back makes an
// immediate read see the write even though the replica is lagging.
func TestConsistencyTokenOverHTTP(t *testing.T) {
	e := setup(t)
	replica := testutil.Replica(t, e.pool)
	st := store.New(e.pool).WithReplica(replica, nil)
	api := httptest.NewServer(httpapi.New(httpapi.Deps{
		Users: st, Posts: st, Search: e.es, Media: mustLocal(t), Consistency: st,
		Tokens: auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour), Log: quiet,
	}))
	defer api.Close()
	e.api = api
	alice := e.login("alice")

	req, _ := http.NewRequest("POST", api.URL+"/posts", strings.NewReader(`{"message":"read me back"}`))
	req.Header.Set("Authorization", "Bearer "+alice)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	token := resp.Header.Get("X-Consistency-Token")
	if resp.StatusCode != http.StatusCreated || token == "" {
		t.Fatalf("create: %d token=%q", resp.StatusCode, token)
	}

	list := func(tok string) int {
		req, _ := http.NewRequest("GET", api.URL+"/posts?author=alice", nil)
		if tok != "" {
			req.Header.Set("X-Consistency-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
		return len(body["items"].([]any))
	}
	if n := list(""); n != 0 {
		t.Fatalf("without a token the lagging replica should not have the post yet (got %d); is the replica delayed?", n)
	}
	if n := list(token); n != 1 {
		t.Fatalf("with the token the post must be visible, got %d", n)
	}
	// Reads never get a token header; failed writes don't either.
	bad, _ := http.NewRequest("POST", api.URL+"/posts", strings.NewReader(`{"message":""}`))
	bad.Header.Set("Authorization", "Bearer "+alice)
	bad.Header.Set("Content-Type", "application/json")
	r2, _ := http.DefaultClient.Do(bad)
	r2.Body.Close()
	if r2.StatusCode != 400 || r2.Header.Get("X-Consistency-Token") != "" {
		t.Fatalf("failed write: %d token=%q", r2.StatusCode, r2.Header.Get("X-Consistency-Token"))
	}
}
