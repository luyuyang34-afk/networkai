package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"networkai/internal/auth"
	"networkai/internal/feed"
	"networkai/internal/metrics"
	"networkai/internal/search"
	"networkai/internal/store"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func init() { auth.Cost = bcrypt.MinCost }

// ---------- fakes ----------

type fakeStore struct {
	mu     sync.Mutex
	users  map[string]store.User
	posts  map[uuid.UUID]store.Post
	nextID int64
	failDB bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{users: map[string]store.User{}, posts: map[uuid.UUID]store.Post{}}
}

func (f *fakeStore) CreateUser(_ context.Context, u, h string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[u]; ok {
		return store.User{}, store.ErrConflict
	}
	f.nextID++
	usr := store.User{ID: f.nextID, Username: u, PasswordHash: h, CreatedAt: time.Now()}
	f.users[u] = usr
	return usr, nil
}

func (f *fakeStore) GetUserByUsername(_ context.Context, u string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	usr, ok := f.users[u]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return usr, nil
}

func (f *fakeStore) CreatePost(_ context.Context, np store.NewPost) (store.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDB {
		return store.Post{}, fmt.Errorf("db down")
	}
	p := store.Post{ID: np.ID, AuthorID: np.AuthorID, Message: np.Message, MediaType: np.MediaType,
		MediaKey: np.MediaKey, MediaURL: np.MediaURL, Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.posts[p.ID] = p
	return p, nil
}

func (f *fakeStore) owned(id uuid.UUID, author int64) (store.Post, error) {
	p, ok := f.posts[id]
	if !ok || p.DeletedAt != nil {
		return p, store.ErrNotFound
	}
	if p.AuthorID != author {
		return p, store.ErrForbidden
	}
	return p, nil
}

func (f *fakeStore) UpdatePostMessage(_ context.Context, id uuid.UUID, author int64, m string) (store.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.owned(id, author)
	if err != nil {
		return p, err
	}
	p.Message = m
	f.posts[id] = p
	return p, nil
}

func (f *fakeStore) DeletePost(_ context.Context, id uuid.UUID, author int64) (store.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.owned(id, author)
	if err != nil {
		return p, err
	}
	now := time.Now()
	p.DeletedAt = &now
	f.posts[id] = p
	return p, nil
}

func (f *fakeStore) GetPost(_ context.Context, id uuid.UUID) (store.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.posts[id]
	if !ok || p.DeletedAt != nil {
		return store.Post{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) ListPosts(context.Context, store.ListParams) ([]store.Post, *store.PageCursor, error) {
	return nil, nil, nil
}

type fakeSearch struct{ err error }

func (s fakeSearch) Search(context.Context, search.Query) (search.Result, error) {
	return search.Result{}, s.err
}

type fakeMedia struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *fakeMedia) Put(_ context.Context, key string, r io.Reader, _ string) (string, error) {
	b, _ := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return "http://media/" + key, nil
}

func (m *fakeMedia) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// ---------- harness ----------

type harness struct {
	t      *testing.T
	h      http.Handler
	st     *fakeStore
	media  *fakeMedia
	tokens *auth.Tokens
}

func newHarness(t *testing.T, searchErr error) *harness {
	st := newFakeStore()
	fm := &fakeMedia{objects: map[string][]byte{}}
	tk := auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour)
	h := New(Deps{
		Users: st, Posts: st, Search: fakeSearch{err: searchErr}, Media: fm, Tokens: tk,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), CORSOrigins: []string{"https://app.example"},
	})
	return &harness{t: t, h: h, st: st, media: fm, tokens: tk}
}

func (h *harness) req(method, path, token string, body io.Reader, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) jsonReq(method, path, token string, v any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	return h.req(method, path, token, bytes.NewReader(b), "application/json")
}

func (h *harness) tokenFor(name string) string {
	u, err := h.st.CreateUser(context.Background(), name, "x")
	if err != nil {
		h.t.Fatal(err)
	}
	tok, _, _ := h.tokens.Issue(auth.Principal{UserID: u.ID, Username: u.Username})
	return tok
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("error response is not the standard JSON shape: %q", w.Body.String())
	}
	if b.RequestID == "" {
		t.Error("error response missing request_id")
	}
	return b.Error.Code
}

func multipartBody(t *testing.T, message string, filename string, content []byte) (io.Reader, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if message != "" {
		mw.WriteField("message", message) //nolint:errcheck
	}
	if filename != "" {
		fw, _ := mw.CreateFormFile("media_file", filename)
		fw.Write(content) //nolint:errcheck
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// ---------- tests ----------

func TestSignupValidation(t *testing.T) {
	cases := []struct {
		user, pw string
		code     int
	}{
		{"alice", "password123", 201},
		{"a_b_9", "password123", 201},
		// The original regex only rejected single-character names; these
		// all used to be accepted.
		{"Alice", "password123", 400},
		{"al ice", "password123", 400},
		{"al", "password123", 400},
		{"alice<script>", "password123", 400},
		{strings.Repeat("a", 21), "password123", 400},
		{"x", "password123", 400},
		{"bobby", "short", 400},
		{"bobby", strings.Repeat("p", 73), 400},
	}
	for _, c := range cases {
		t.Run(c.user+"/"+fmt.Sprint(len(c.pw)), func(t *testing.T) {
			h := newHarness(t, nil)
			w := h.jsonReq("POST", "/users", "", map[string]string{"username": c.user, "password": c.pw})
			if w.Code != c.code {
				t.Fatalf("status %d, want %d: %s", w.Code, c.code, w.Body)
			}
		})
	}
}

func TestSignupStoresHashNotPassword(t *testing.T) {
	h := newHarness(t, nil)
	h.jsonReq("POST", "/users", "", map[string]string{"username": "alice", "password": "password123"})
	u := h.st.users["alice"]
	if u.PasswordHash == "password123" || !auth.CheckPassword(u.PasswordHash, "password123") {
		t.Fatalf("stored %q; want a bcrypt hash", u.PasswordHash)
	}
	if w := h.jsonReq("POST", "/users", "", map[string]string{"username": "alice", "password": "password456"}); w.Code != 409 || errCode(t, w) != "username_taken" {
		t.Fatalf("duplicate signup: %d %s", w.Code, w.Body)
	}
}

// Wrong password and unknown user must be indistinguishable to the client.
func TestLoginDoesNotRevealWhetherUserExists(t *testing.T) {
	h := newHarness(t, nil)
	h.jsonReq("POST", "/users", "", map[string]string{"username": "alice", "password": "password123"})

	ok := h.jsonReq("POST", "/auth/login", "", map[string]string{"username": "alice", "password": "password123"})
	if ok.Code != 200 {
		t.Fatalf("valid login: %d %s", ok.Code, ok.Body)
	}
	wrongPw := h.jsonReq("POST", "/auth/login", "", map[string]string{"username": "alice", "password": "nope-nope"})
	noUser := h.jsonReq("POST", "/auth/login", "", map[string]string{"username": "nobody", "password": "nope-nope"})
	if wrongPw.Code != 401 || noUser.Code != 401 {
		t.Fatalf("codes %d / %d, want 401 / 401", wrongPw.Code, noUser.Code)
	}
	if errCode(t, wrongPw) != errCode(t, noUser) {
		t.Fatal("error codes differ between wrong password and unknown user")
	}
}

func TestWritesRequireAuth(t *testing.T) {
	h := newHarness(t, nil)
	for _, c := range []struct{ method, path string }{
		{"POST", "/posts"}, {"PATCH", "/posts/" + uuid.NewString()}, {"DELETE", "/posts/" + uuid.NewString()},
	} {
		for _, tok := range []string{"", "garbage"} {
			w := h.req(c.method, c.path, tok, nil, "")
			if w.Code != 401 || errCode(t, w) != "unauthenticated" {
				t.Errorf("%s %s token=%q: %d %s", c.method, c.path, tok, w.Code, w.Body)
			}
		}
	}
}

func TestOwnershipIsEnforced(t *testing.T) {
	h := newHarness(t, nil)
	alice, bob := h.tokenFor("alice"), h.tokenFor("bobby")
	body, ct := multipartBody(t, "alice's post", "", nil)
	w := h.req("POST", "/posts", alice, body, ct)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var p postResponse
	json.Unmarshal(w.Body.Bytes(), &p) //nolint:errcheck

	if w := h.jsonReq("PATCH", "/posts/"+p.ID, bob, map[string]string{"message": "pwned"}); w.Code != 403 {
		t.Errorf("bob edit: %d", w.Code)
	}
	if w := h.req("DELETE", "/posts/"+p.ID, bob, nil, ""); w.Code != 403 {
		t.Errorf("bob delete: %d", w.Code)
	}
	if w := h.req("DELETE", "/posts/"+uuid.NewString(), alice, nil, ""); w.Code != 404 {
		t.Errorf("delete missing: %d", w.Code)
	}
	if w := h.req("DELETE", "/posts/not-a-uuid", alice, nil, ""); w.Code != 404 {
		t.Errorf("delete bad id: %d", w.Code)
	}
	if w := h.req("DELETE", "/posts/"+p.ID, alice, nil, ""); w.Code != 204 {
		t.Errorf("owner delete: %d", w.Code)
	}
}

func TestUploadSniffsContent(t *testing.T) {
	h := newHarness(t, nil)
	alice := h.tokenFor("alice")

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")
	body, ct := multipartBody(t, "pic", "photo.png", png)
	w := h.req("POST", "/posts", alice, body, ct)
	if w.Code != 201 {
		t.Fatalf("png upload: %d %s", w.Code, w.Body)
	}
	var p postResponse
	json.Unmarshal(w.Body.Bytes(), &p) //nolint:errcheck
	if p.MediaType != "image" || !bytes.Equal(h.media.objects[p.ID+".png"], png) {
		t.Fatalf("stored media wrong: type=%q objects=%v", p.MediaType, len(h.media.objects))
	}

	body, ct = multipartBody(t, "evil", "evil.jpg", []byte("<html><script>alert(1)</script></html>"))
	if w := h.req("POST", "/posts", alice, body, ct); w.Code != 415 {
		t.Fatalf("html disguised as jpg: %d, want 415", w.Code)
	}

	body, ct = multipartBody(t, "", "", nil)
	if w := h.req("POST", "/posts", alice, body, ct); w.Code != 400 {
		t.Fatalf("empty post: %d, want 400", w.Code)
	}
}

func TestCreateTextPostWithJSON(t *testing.T) {
	h := newHarness(t, nil)
	alice := h.tokenFor("alice")
	if w := h.jsonReq("POST", "/posts", alice, map[string]string{"message": "  hello json  "}); w.Code != 201 ||
		!strings.Contains(w.Body.String(), `"message":"hello json"`) {
		t.Fatalf("json post: %d %s", w.Code, w.Body)
	}
	for _, body := range []any{map[string]string{"message": "   "}, map[string]any{"message": "x", "extra": 1}} {
		if w := h.jsonReq("POST", "/posts", alice, body); w.Code != 400 {
			t.Errorf("body %v: %d, want 400", body, w.Code)
		}
	}
	if w := h.req("POST", "/posts", alice, strings.NewReader("message=x"), "application/x-www-form-urlencoded"); w.Code != 400 {
		t.Errorf("urlencoded: %d, want 400", w.Code)
	}
}

// If the database write fails after the file was uploaded, the file must be
// removed (compensating action) so storage does not fill with orphans.
func TestUploadIsCleanedUpWhenDBWriteFails(t *testing.T) {
	h := newHarness(t, nil)
	alice := h.tokenFor("alice")
	h.st.failDB = true
	body, ct := multipartBody(t, "pic", "photo.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	if w := h.req("POST", "/posts", alice, body, ct); w.Code != 500 || errCode(t, w) != "internal" {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if len(h.media.objects) != 0 {
		t.Fatalf("orphaned media left behind: %d objects", len(h.media.objects))
	}
}

func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	h := newHarness(t, nil)
	alice := h.tokenFor("alice")
	h.st.failDB = true
	body, ct := multipartBody(t, "hello", "", nil)
	w := h.req("POST", "/posts", alice, body, ct)
	if strings.Contains(w.Body.String(), "db down") {
		t.Fatalf("internal error text leaked to client: %s", w.Body)
	}
}

func TestSearchErrorMapping(t *testing.T) {
	if w := newHarness(t, search.ErrUnavailable).req("GET", "/posts/search?q=x", "", nil, ""); w.Code != 503 || errCode(t, w) != "search_unavailable" {
		t.Errorf("ES down: %d %s", w.Code, w.Body)
	}
	if w := newHarness(t, search.ErrBadCursor).req("GET", "/posts/search?cursor=x", "", nil, ""); w.Code != 400 {
		t.Errorf("bad cursor: %d", w.Code)
	}
	h := newHarness(t, nil)
	for _, q := range []string{"limit=0", "limit=101", "limit=abc", "type=audio"} {
		if w := h.req("GET", "/posts/search?"+q, "", nil, ""); w.Code != 400 {
			t.Errorf("%s: %d, want 400", q, w.Code)
		}
	}
}

func TestCORSOnlyAllowsConfiguredOrigins(t *testing.T) {
	h := newHarness(t, nil)
	pre := func(origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("OPTIONS", "/posts", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		return w
	}
	if got := pre("https://app.example").Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("allowed origin got %q", got)
	}
	if got := pre("https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin got %q", got)
	}
}

func TestRequestIDIsPropagated(t *testing.T) {
	h := newHarness(t, nil)
	r := httptest.NewRequest("GET", "/posts/"+uuid.NewString(), nil)
	r.Header.Set("X-Request-ID", "trace-123")
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Header().Get("X-Request-ID") != "trace-123" || !strings.Contains(w.Body.String(), "trace-123") {
		t.Fatalf("request id not propagated: headers=%v body=%s", w.Header(), w.Body)
	}
}

func TestPageCursorRoundTrip(t *testing.T) {
	c := store.PageCursor{CreatedAt: time.Now().UTC().Truncate(time.Microsecond), ID: uuid.New()}
	got, err := decodePageCursor(encodePageCursor(c))
	if err != nil || !got.CreatedAt.Equal(c.CreatedAt) || got.ID != c.ID {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"", "!!!", "e30"} { // e30 = base64("{}")
		if _, err := decodePageCursor(bad); err == nil {
			t.Errorf("cursor %q should be rejected", bad)
		}
	}
}

func TestMetricsUseRoutePatternNotRawPath(t *testing.T) {
	m := metrics.New()
	h := New(Deps{Users: newFakeStore(), Posts: newFakeStore(), Search: fakeSearch{}, Metrics: m,
		Tokens: auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil))})
	id := uuid.NewString()
	for _, p := range []string{"/posts/" + id, "/posts/" + uuid.NewString(), "/no/such/route"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	if !strings.Contains(body, `networkai_http_requests_total{route="GET /posts/{id}",status="404"} 2`) {
		t.Errorf("expected both post lookups under one route-pattern series:\n%s", grepLines(body, "http_requests_total"))
	}
	if strings.Contains(body, id) {
		t.Error("raw id leaked into metric labels (cardinality explosion)")
	}
	if !strings.Contains(body, `route="unmatched"`) {
		t.Error("unknown paths should share a single 'unmatched' series")
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

type fakeFeed struct{ err error }

func (f fakeFeed) ForYou(context.Context, int64, string, int) ([]feed.Item, error) { return nil, f.err }

func TestFeedSheddingMapsToRetryableError(t *testing.T) {
	h := newHarness(t, nil)
	h.h = New(Deps{Users: h.st, Posts: h.st, Search: fakeSearch{}, Media: h.media, Tokens: h.tokens,
		Feed: fakeFeed{err: feed.ErrBusy}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	w := h.req("GET", "/feed", h.tokenFor("alice"), nil, "")
	if w.Code != 503 || errCode(t, w) != "feed_busy" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("busy feed: %d %v %s", w.Code, w.Header(), w.Body)
	}
}
