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
	"networkai/internal/httpapi"
	"networkai/internal/indexer"
	"networkai/internal/media"
	"networkai/internal/outbox"
	"networkai/internal/search"
	"networkai/internal/store"
	"networkai/internal/testutil"

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
	api := httptest.NewServer(httpapi.New(httpapi.Deps{
		Users: st, Posts: st, Search: es, Media: ms,
		Ready:  readiness{Store: st, pool: pool},
		Tokens: auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour),
		Log:    quiet,
	}))
	t.Cleanup(api.Close)

	ix := &indexer.Indexer{Posts: st, Index: es}
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
