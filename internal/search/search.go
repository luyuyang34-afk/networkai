// Package search is a small Elasticsearch client for the posts index.
//
// It talks to the REST API with net/http directly. The surface we need is
// tiny (create index, index a document, search), and seeing the raw requests
// makes the Elasticsearch concepts easier to learn than a generated client.
package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAlias is what the application reads and writes. It points at a
// versioned physical index (posts_v1) so the mapping can change later through
// a zero-downtime reindex: build posts_v2, then swap the alias.
const DefaultAlias = "posts"

var ErrUnavailable = errors.New("search backend unavailable")

type Client struct {
	alias    string
	baseURL  string
	username string
	password string
	http     *http.Client
}

func New(baseURL, username, password string) *Client {
	return &Client{
		alias:    DefaultAlias,
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// WithAlias returns a copy of the client using a different index alias
// (tests use this to get an isolated index each).
func (c *Client) WithAlias(alias string) *Client {
	cp := *c
	cp.alias = alias
	return &cp
}

func (c *Client) physicalName() string { return c.alias + "_v1" }

// Alias returns the alias this client reads and writes.
func (c *Client) Alias() string { return c.alias }

// Sink is anything that accepts versioned document writes.
type Sink interface {
	Upsert(ctx context.Context, doc Doc, version int64) (bool, error)
	BulkUpsert(ctx context.Context, docs []VersionedDoc) (BulkResult, error)
}

// Into returns a client whose writes target a specific physical index.
func (c *Client) Into(index string) Sink { return c.WithAlias(index) }

// Doc is the denormalised search document for one post.
type Doc struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Message   string    `json:"message"`
	MediaType string    `json:"media_type"`
	MediaURL  string    `json:"media_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Deleted posts stay in the index as tombstones (filtered out at query
	// time). A real delete would let a late, stale write re-create the doc;
	// a tombstone carries a version that rejects it. See ADR 0002.
	Deleted bool `json:"deleted"`
	// Embedding is the post's semantic vector (see package embed). Omitted
	// when no embedding model is configured or the text has no known terms.
	Embedding []float32 `json:"embedding,omitempty"`
}

// EmbeddingDims must match the trained model (models/lsa.json).
const EmbeddingDims = 64

// embeddingMapping is added to the index with PUT _mapping. Adding a new
// field is one of the few mapping changes Elasticsearch allows in place;
// changing an existing field's type would need a new index and an alias swap.
var embeddingMapping = fmt.Sprintf(`{"properties":{"embedding":{"type":"dense_vector","dims":%d,"index":true,"similarity":"cosine"}}}`, EmbeddingDims)

// indexDefinition is the settings + mapping for the posts index.
//
//   - message uses a custom English analyzer (lowercase, ASCII folding, stop
//     words, stemming) so "running" matches "run" and "café" matches "cafe".
//   - message.exact keeps an unstemmed copy so exact wording ranks higher.
//   - author/media_type are keyword: exact-match filters, not full text.
//   - dynamic: strict rejects documents with unexpected fields instead of
//     silently growing the mapping.
const indexDefinition = `{
  "settings": {
    "number_of_shards": 1,
    "number_of_replicas": 0,
    "analysis": {
      "filter": {
        "english_possessive": {"type": "stemmer", "language": "possessive_english"},
        "english_stop":       {"type": "stop", "stopwords": "_english_"},
        "english_stemmer":    {"type": "stemmer", "language": "english"}
      },
      "analyzer": {
        "post_text": {
          "type": "custom",
          "tokenizer": "standard",
          "filter": ["english_possessive", "lowercase", "asciifolding", "english_stop", "english_stemmer"]
        },
        "post_exact": {
          "type": "custom",
          "tokenizer": "standard",
          "filter": ["lowercase", "asciifolding"]
        }
      }
    }
  },
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "id":         {"type": "keyword"},
      "author":     {"type": "keyword"},
      "message":    {"type": "text", "analyzer": "post_text",
                     "fields": {"exact": {"type": "text", "analyzer": "post_exact"}}},
      "media_type": {"type": "keyword"},
      "media_url":  {"type": "keyword", "index": false},
      "created_at": {"type": "date"},
      "updated_at": {"type": "date"},
      "deleted":    {"type": "boolean"}
    }
  },
  "aliases": {"__ALIAS__": {}}
}`

func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return err
	}
	return expect(resp, http.StatusOK)
}

// EnsureIndex creates the physical index and alias if they do not exist.
func (c *Client) EnsureIndex(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodHead, "/"+c.alias, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return c.ensureEmbeddingField(ctx)
	}
	def := strings.Replace(indexDefinition, "__ALIAS__", c.alias, 1)
	resp, err = c.do(ctx, http.MethodPut, "/"+c.physicalName(), strings.NewReader(def))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusBadRequest {
		body := readBody(resp)
		if strings.Contains(body, "resource_already_exists_exception") {
			return c.ensureEmbeddingField(ctx) // another process won the race
		}
		return fmt.Errorf("create index: %s", body)
	}
	if err := expect(resp, http.StatusOK); err != nil {
		return err
	}
	return c.ensureEmbeddingField(ctx)
}

// CreateIndex creates a new physical index with the current definition but
// without attaching the alias (used by blue/green rebuilds).
func (c *Client) CreateIndex(ctx context.Context, name string) error {
	def := strings.Replace(indexDefinition, `"aliases": {"__ALIAS__": {}}`, `"aliases": {}`, 1)
	resp, err := c.do(ctx, http.MethodPut, "/"+name, strings.NewReader(def))
	if err != nil {
		return err
	}
	if err := expect(resp, http.StatusOK); err != nil {
		return err
	}
	resp, err = c.do(ctx, http.MethodPut, "/"+name+"/_mapping", strings.NewReader(embeddingMapping))
	if err != nil {
		return err
	}
	return expect(resp, http.StatusOK)
}

// SwapAlias atomically points the alias at newIndex and detaches it from
// every other index, returning the indices it was detached from. Readers
// never see a moment with no index or two indices behind the alias.
func (c *Client) SwapAlias(ctx context.Context, newIndex string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/_alias/"+c.alias, nil)
	if err != nil {
		return nil, err
	}
	var old []string
	if resp.StatusCode == http.StatusOK {
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m) //nolint:errcheck
		resp.Body.Close()
		for idx := range m {
			if idx != newIndex {
				old = append(old, idx)
			}
		}
	} else {
		resp.Body.Close()
	}
	actions := []any{map[string]any{"add": map[string]any{"index": newIndex, "alias": c.alias}}}
	for _, idx := range old {
		actions = append(actions, map[string]any{"remove": map[string]any{"index": idx, "alias": c.alias}})
	}
	body, _ := json.Marshal(map[string]any{"actions": actions})
	resp, err = c.do(ctx, http.MethodPost, "/_aliases", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return old, expect(resp, http.StatusOK)
}

func (c *Client) DeleteIndex(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/"+name, nil)
	if err != nil {
		return err
	}
	return expect(resp, http.StatusOK)
}

// ensureEmbeddingField adds the embedding field to an existing index
// (idempotent: re-putting an identical mapping is a no-op).
func (c *Client) ensureEmbeddingField(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPut, "/"+c.alias+"/_mapping", strings.NewReader(embeddingMapping))
	if err != nil {
		return err
	}
	return expect(resp, http.StatusOK)
}

// Embeddings returns the stored embedding for each id that has one (_mget).
func (c *Client) Embeddings(ctx context.Context, ids []string) (map[string][]float32, error) {
	if len(ids) == 0 {
		return map[string][]float32{}, nil
	}
	body, _ := json.Marshal(map[string]any{"ids": ids})
	resp, err := c.do(ctx, http.MethodPost, "/"+c.alias+"/_mget?_source_includes=embedding", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mget: status %d: %s", resp.StatusCode, readBody(resp))
	}
	defer resp.Body.Close()
	var out struct {
		Docs []struct {
			ID     string `json:"_id"`
			Found  bool   `json:"found"`
			Source struct {
				Embedding []float32 `json:"embedding"`
			} `json:"_source"`
		} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := make(map[string][]float32, len(out.Docs))
	for _, d := range out.Docs {
		if d.Found && len(d.Source.Embedding) > 0 {
			res[d.ID] = d.Source.Embedding
		}
	}
	return res, nil
}

// CandidateQuery describes one retrieval source for the personalised feed.
// Exactly one of Vector (kNN) or LikeIDs (more_like_this) may be set; with
// neither, the newest posts are returned.
type CandidateQuery struct {
	Since         time.Time
	Size          int
	Vector        []float32
	LikeIDs       []string
	ExcludeIDs    []string
	ExcludeAuthor string
}

// Candidates runs one retrieval source. Results include embeddings, which the
// ranker needs for its similarity feature.
func (c *Client) Candidates(ctx context.Context, q CandidateQuery) ([]Doc, error) {
	filter := []any{
		map[string]any{"term": map[string]any{"deleted": false}},
		map[string]any{"range": map[string]any{"created_at": map[string]any{"gte": q.Since.UTC().Format(time.RFC3339)}}},
	}
	var mustNot []any
	if len(q.ExcludeIDs) > 0 {
		mustNot = append(mustNot, map[string]any{"ids": map[string]any{"values": q.ExcludeIDs}})
	}
	if q.ExcludeAuthor != "" {
		mustNot = append(mustNot, map[string]any{"term": map[string]any{"author": q.ExcludeAuthor}})
	}
	boolQ := map[string]any{"filter": filter, "must_not": mustNot}
	body := map[string]any{"size": q.Size, "track_total_hits": false}

	switch {
	case len(q.Vector) > 0:
		// Approximate kNN (HNSW). Filters apply *during* the graph search,
		// so all k results satisfy them.
		body["knn"] = map[string]any{
			"field": "embedding", "query_vector": q.Vector, "k": q.Size,
			"num_candidates": max(q.Size*3, 100),
			"filter":         map[string]any{"bool": boolQ},
		}
	case len(q.LikeIDs) > 0:
		like := make([]any, len(q.LikeIDs))
		for i, id := range q.LikeIDs {
			like[i] = map[string]any{"_index": c.alias, "_id": id}
		}
		boolQ["must"] = []any{map[string]any{"more_like_this": map[string]any{
			"fields": []string{"message"}, "like": like,
			"min_term_freq": 1, "min_doc_freq": 2, "max_query_terms": 25,
		}}}
		body["query"] = map[string]any{"bool": boolQ}
	default:
		body["query"] = map[string]any{"bool": boolQ}
		body["sort"] = []any{map[string]any{"created_at": "desc"}}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/"+c.alias+"/_search", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("candidates: status %d: %s", resp.StatusCode, readBody(resp))
	}
	defer resp.Body.Close()
	var sr struct {
		Hits struct {
			Hits []struct {
				Source Doc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	docs := make([]Doc, len(sr.Hits.Hits))
	for i, h := range sr.Hits.Hits {
		docs[i] = h.Source
	}
	return docs, nil
}

// Upsert writes doc with an external version. Elasticsearch only accepts the
// write if version is greater than the stored one, so replays and
// out-of-order deliveries are harmless no-ops. It reports whether the write
// was applied (false means a newer version was already indexed).
func (c *Client) Upsert(ctx context.Context, doc Doc, version int64) (applied bool, err error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("/%s/_doc/%s?version=%d&version_type=external",
		c.alias, url.PathEscape(doc.ID), version)
	resp, err := c.do(ctx, http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		resp.Body.Close()
		return true, nil
	case http.StatusConflict:
		resp.Body.Close()
		return false, nil
	default:
		return false, fmt.Errorf("index doc %s: status %d: %s", doc.ID, resp.StatusCode, readBody(resp))
	}
}

// VersionedDoc pairs a document with its external version for BulkUpsert.
type VersionedDoc struct {
	Doc     Doc
	Version int64
}

// BulkResult counts per-document outcomes of a bulk request.
type BulkResult struct {
	Applied int
	Stale   int // rejected because a newer version is already indexed (409)
}

// BulkUpsert writes many documents in one request using the _bulk API, with
// the same external-version semantics as Upsert. One round trip per batch
// instead of per document is what makes a full reindex fast.
func (c *Client) BulkUpsert(ctx context.Context, docs []VersionedDoc) (BulkResult, error) {
	if len(docs) == 0 {
		return BulkResult{}, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // Encode appends the newline NDJSON needs
	for _, d := range docs {
		meta := map[string]any{"index": map[string]any{
			"_index": c.alias, "_id": d.Doc.ID, "version": d.Version, "version_type": "external",
		}}
		if err := enc.Encode(meta); err != nil {
			return BulkResult{}, err
		}
		if err := enc.Encode(d.Doc); err != nil {
			return BulkResult{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/_bulk", &buf)
	if err != nil {
		return BulkResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.send(req)
	if err != nil {
		return BulkResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return BulkResult{}, fmt.Errorf("bulk: status %d: %s", resp.StatusCode, readBody(resp))
	}
	defer resp.Body.Close()
	var out struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			ID     string          `json:"_id"`
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return BulkResult{}, fmt.Errorf("decode bulk response: %w", err)
	}
	var res BulkResult
	var firstErr error
	for _, item := range out.Items {
		for _, r := range item {
			switch {
			case r.Status >= 200 && r.Status < 300:
				res.Applied++
			case r.Status == http.StatusConflict:
				res.Stale++
			case r.Status == http.StatusTooManyRequests || r.Status >= 500:
				if firstErr == nil {
					firstErr = fmt.Errorf("%w: bulk item %s: status %d", ErrUnavailable, r.ID, r.Status)
				}
			default:
				if firstErr == nil {
					firstErr = fmt.Errorf("bulk item %s: status %d: %s", r.ID, r.Status, r.Error)
				}
			}
		}
	}
	return res, firstErr
}

// Refresh makes recent writes visible to search immediately. Tests and the
// demo use it; production relies on the default 1s refresh interval.
func (c *Client) Refresh(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPost, "/"+c.alias+"/_refresh", nil)
	if err != nil {
		return err
	}
	return expect(resp, http.StatusOK)
}

// PurgeTombstones permanently removes deleted-post tombstones last updated
// before olderThan ago, returning how many were removed.
//
// This is safe because the indexer always re-reads PostgreSQL: if a very late
// event for a purged post is ever processed, PostgreSQL still says "deleted"
// and the indexer just writes a fresh tombstone. The post cannot come back.
func (c *Client) PurgeTombstones(ctx context.Context, olderThan time.Duration) (int64, error) {
	body := fmt.Sprintf(`{"query":{"bool":{"filter":[
		{"term":{"deleted":true}},
		{"range":{"updated_at":{"lt":"now-%ds"}}}]}}}`, int64(olderThan.Seconds()))
	resp, err := c.do(ctx, http.MethodPost, "/"+c.alias+"/_delete_by_query?conflicts=proceed&refresh=true",
		strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("purge tombstones: status %d: %s", resp.StatusCode, readBody(resp))
	}
	defer resp.Body.Close()
	var out struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Deleted, nil
}

type Query struct {
	Text      string // full-text query on message; empty = newest first
	Author    string // exact username filter
	MediaType string // none|image|video
	Limit     int
	Cursor    string // opaque, from a previous Result.NextCursor
}

type Result struct {
	Docs       []Doc
	NextCursor string
}

var ErrBadCursor = errors.New("invalid cursor")

// Search runs a relevance-ranked query with search_after pagination.
//
// search_after is the deep-pagination tool Elasticsearch recommends instead
// of from/size: it resumes after the sort values of the last hit, so page 500
// costs the same as page 1. The sort always ends with id as a unique
// tie-breaker so no hit is skipped or repeated between pages.
func (c *Client) Search(ctx context.Context, q Query) (Result, error) {
	filters := []any{map[string]any{"term": map[string]any{"deleted": false}}}
	if q.Author != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"author": q.Author}})
	}
	if q.MediaType != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"media_type": q.MediaType}})
	}

	boolQ := map[string]any{"filter": filters}
	sort := []any{map[string]any{"created_at": "desc"}, map[string]any{"id": "asc"}}
	if strings.TrimSpace(q.Text) != "" {
		boolQ["must"] = []any{map[string]any{
			"multi_match": map[string]any{
				"query":    q.Text,
				"fields":   []string{"message", "message.exact^2"},
				"type":     "most_fields",
				"operator": "and",
			},
		}}
		sort = append([]any{map[string]any{"_score": "desc"}}, sort...)
	}

	body := map[string]any{
		"_source":          map[string]any{"excludes": []string{"embedding"}},
		"size":             q.Limit + 1, // one extra to detect a next page
		"query":            map[string]any{"bool": boolQ},
		"sort":             sort,
		"track_total_hits": false,
	}
	if q.Cursor != "" {
		after, err := decodeCursor(q.Cursor)
		if err != nil {
			return Result{}, err
		}
		body["search_after"] = after
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/"+c.alias+"/_search", bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode == http.StatusBadRequest && q.Cursor != "" {
		resp.Body.Close()
		return Result{}, ErrBadCursor
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("search: status %d: %s", resp.StatusCode, readBody(resp))
	}
	defer resp.Body.Close()

	var sr struct {
		Hits struct {
			Hits []struct {
				Source Doc             `json:"_source"`
				Sort   json.RawMessage `json:"sort"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return Result{}, fmt.Errorf("decode search response: %w", err)
	}

	hits := sr.Hits.Hits
	var res Result
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
		res.NextCursor = base64.RawURLEncoding.EncodeToString(hits[len(hits)-1].Sort)
	}
	res.Docs = make([]Doc, len(hits))
	for i, h := range hits {
		res.Docs[i] = h.Source
	}
	return res, nil
}

func decodeCursor(s string) (json.RawMessage, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
		return nil, ErrBadCursor
	}
	return raw, nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req)
}

// send executes req with auth and maps transport errors, 5xx and 429 to
// ErrUnavailable.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// 5xx and 429 (Elasticsearch's backpressure signal) mean "try again
	// later", not "this request is wrong".
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		body := readBody(resp)
		return nil, fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}
	return resp, nil
}

func expect(resp *http.Response, code int) error {
	if resp.StatusCode != code {
		return fmt.Errorf("elasticsearch: unexpected status %d: %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()
	return nil
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return strconv.Quote(string(b))
}
