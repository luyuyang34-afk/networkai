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
}

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
		return nil
	}
	def := strings.Replace(indexDefinition, "__ALIAS__", c.alias, 1)
	resp, err = c.do(ctx, http.MethodPut, "/"+c.physicalName(), strings.NewReader(def))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusBadRequest {
		body := readBody(resp)
		if strings.Contains(body, "resource_already_exists_exception") {
			return nil // another process won the race
		}
		return fmt.Errorf("create index: %s", body)
	}
	return expect(resp, http.StatusOK)
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

// Refresh makes recent writes visible to search immediately. Tests and the
// demo use it; production relies on the default 1s refresh interval.
func (c *Client) Refresh(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPost, "/"+c.alias+"/_refresh", nil)
	if err != nil {
		return err
	}
	return expect(resp, http.StatusOK)
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
