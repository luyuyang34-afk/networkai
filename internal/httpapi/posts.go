package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"networkai/internal/auth"
	"networkai/internal/feed"
	"networkai/internal/media"
	"networkai/internal/search"
	"networkai/internal/store"

	"github.com/google/uuid"
)

const (
	maxMessageChars = 5000
	defaultLimit    = 20
	maxLimit        = 100
)

type postResponse struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Message   string    `json:"message"`
	MediaType string    `json:"media_type"`
	MediaURL  string    `json:"media_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	LikeCount int       `json:"like_count"`
	Liked     bool      `json:"liked"`
}

type pageResponse struct {
	Items      []postResponse `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

func toResponse(p store.Post) postResponse {
	return postResponse{
		ID: p.ID.String(), Author: p.Author, Message: p.Message, MediaType: p.MediaType,
		MediaURL: p.MediaURL, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func docToResponse(d search.Doc) postResponse {
	return postResponse{
		ID: d.ID, Author: d.Author, Message: d.Message, MediaType: d.MediaType,
		MediaURL: d.MediaURL, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

func parseLimit(r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultLimit, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxLimit {
		return 0, false
	}
	return n, true
}

func validMessage(m string) bool {
	return utf8.ValidString(m) && utf8.RuneCountInString(m) <= maxMessageChars
}

// mediaExts is the allow-list of file extensions kept in storage keys.
var mediaExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".mp4": true, ".mov": true, ".webm": true, ".avi": true,
}

// createPost accepts either JSON {"message": "..."} for a text-only post, or
// multipart/form-data with `message` and an optional `media_file`.
//
// Ordering matters: the file is uploaded first and the database row second.
// If the database write fails we delete the file (a compensating action). The
// reverse order could leave a post pointing at a file that never arrived.
func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		s.createTextPost(w, r, p)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds size limit")
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid_form", "expected multipart/form-data")
		return
	}
	defer r.MultipartForm.RemoveAll() //nolint:errcheck

	message := strings.TrimSpace(r.FormValue("message"))
	if !validMessage(message) {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "message must be valid UTF-8, at most 5000 characters")
		return
	}

	np := store.NewPost{ID: uuid.New(), AuthorID: p.UserID, Message: message, MediaType: "none"}

	file, header, err := r.FormFile("media_file")
	switch {
	case errors.Is(err, http.ErrMissingFile):
		if message == "" {
			writeError(w, r, http.StatusBadRequest, "invalid_input", "a post needs a message or a media_file")
			return
		}
	case err != nil:
		writeError(w, r, http.StatusBadRequest, "invalid_form", "could not read media_file")
		return
	default:
		defer file.Close()
		head := make([]byte, 512)
		n, _ := io.ReadFull(file, head)
		head = head[:n]
		kind, contentType, ok := media.Kind(head, header.Filename)
		if !ok {
			writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media",
				"media_file must be an image or video, got "+contentType)
			return
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !mediaExts[ext] {
			ext = ""
		}
		np.MediaType = kind
		np.MediaKey = np.ID.String() + ext
		np.MediaURL, err = s.media.Put(r.Context(), np.MediaKey, io.MultiReader(bytes.NewReader(head), file), contentType)
		if err != nil {
			internalError(w, r, s.log, err)
			return
		}
	}

	post, err := s.posts.CreatePost(r.Context(), np)
	if err != nil {
		if np.MediaKey != "" {
			s.cleanupMedia(r.Context(), np.MediaKey)
		}
		internalError(w, r, s.log, err)
		return
	}
	w.Header().Set("Location", "/posts/"+post.ID.String())
	writeJSON(w, http.StatusCreated, toResponse(post))
}

func (s *Server) createTextPost(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var body struct {
		Message string `json:"message"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", `body must be {"message": "..."}`)
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" || !validMessage(msg) {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "message must be 1-5000 characters of valid UTF-8")
		return
	}
	post, err := s.posts.CreatePost(r.Context(), store.NewPost{ID: uuid.New(), AuthorID: p.UserID, Message: msg, MediaType: "none"})
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	w.Header().Set("Location", "/posts/"+post.ID.String())
	writeJSON(w, http.StatusCreated, toResponse(post))
}

func (s *Server) cleanupMedia(ctx context.Context, key string) {
	// Use a fresh context: the request may already be cancelled, but the
	// cleanup should still run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.media.Delete(ctx, key); err != nil {
		s.log.Warn("media cleanup failed; object is orphaned", "key", key, "err", err)
	}
}

func (s *Server) postID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "post not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) getPost(w http.ResponseWriter, r *http.Request) {
	id, ok := s.postID(w, r)
	if !ok {
		return
	}
	p, err := s.posts.GetPost(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "post not found")
		return
	}
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	items := []postResponse{toResponse(p)}
	if err := s.attachLikes(r, items); err != nil {
		internalError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, items[0])
}

func (s *Server) updatePost(w http.ResponseWriter, r *http.Request) {
	id, ok := s.postID(w, r)
	if !ok {
		return
	}
	var body struct {
		Message *string `json:"message"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Message == nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", `body must be {"message": "..."}`)
		return
	}
	msg := strings.TrimSpace(*body.Message)
	if !validMessage(msg) {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "message must be valid UTF-8, at most 5000 characters")
		return
	}
	p, _ := auth.FromContext(r.Context())
	post, err := s.posts.UpdatePostMessage(r.Context(), id, p.UserID, msg)
	if s.writeOwnershipError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toResponse(post))
}

func (s *Server) deletePost(w http.ResponseWriter, r *http.Request) {
	id, ok := s.postID(w, r)
	if !ok {
		return
	}
	p, _ := auth.FromContext(r.Context())
	post, err := s.posts.DeletePost(r.Context(), id, p.UserID)
	if s.writeOwnershipError(w, r, err) {
		return
	}
	if post.MediaKey != "" {
		s.cleanupMedia(r.Context(), post.MediaKey)
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeOwnershipError maps store errors for owner-only operations. Returns
// true if it wrote a response.
func (s *Server) writeOwnershipError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "post not found")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "you can only modify your own posts")
	default:
		internalError(w, r, s.log, err)
	}
	return true
}

// listPosts is the chronological feed, served from PostgreSQL (the source of
// truth), so it keeps working even when Elasticsearch is down.
func (s *Server) listPosts(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "limit must be 1-100")
		return
	}
	lp := store.ListParams{Author: r.URL.Query().Get("author"), Limit: limit}
	if c := r.URL.Query().Get("cursor"); c != "" {
		cur, err := decodePageCursor(c)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		lp.After = &cur
	}
	posts, next, err := s.posts.ListPosts(r.Context(), lp)
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	resp := pageResponse{Items: make([]postResponse, len(posts))}
	for i, p := range posts {
		resp.Items[i] = toResponse(p)
	}
	if err := s.attachLikes(r, resp.Items); err != nil {
		internalError(w, r, s.log, err)
		return
	}
	if next != nil {
		resp.NextCursor = encodePageCursor(*next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) searchPosts(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "limit must be 1-100")
		return
	}
	q := r.URL.Query()
	mt := q.Get("type")
	if mt != "" && mt != "none" && mt != "image" && mt != "video" {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "type must be none, image or video")
		return
	}
	res, err := s.search.Search(r.Context(), search.Query{
		Text: q.Get("q"), Author: q.Get("author"), MediaType: mt, Limit: limit, Cursor: q.Get("cursor"),
	})
	s.observeSearch(err)
	switch {
	case errors.Is(err, search.ErrBadCursor):
		writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
		return
	case errors.Is(err, search.ErrUnavailable):
		s.log.WarnContext(r.Context(), "search unavailable", "err", err)
		writeError(w, r, http.StatusServiceUnavailable, "search_unavailable",
			"search is temporarily unavailable; browsing and posting still work")
		return
	case err != nil:
		internalError(w, r, s.log, err)
		return
	}
	resp := pageResponse{Items: make([]postResponse, len(res.Docs)), NextCursor: res.NextCursor}
	for i, d := range res.Docs {
		resp.Items[i] = docToResponse(d)
	}
	if err := s.attachLikes(r, resp.Items); err != nil {
		internalError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) observeSearch(err error) {
	if s.metrics == nil {
		return
	}
	switch {
	case err == nil:
		s.metrics.ObserveSearch("ok")
	case errors.Is(err, search.ErrUnavailable):
		s.metrics.ObserveSearch("unavailable")
	default:
		s.metrics.ObserveSearch("error")
	}
}

// attachLikes fills like_count/liked for a page in one batched query.
func (s *Server) attachLikes(r *http.Request, items []postResponse) error {
	if s.likes == nil || len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	p, _ := auth.FromContext(r.Context())
	counts, mine, err := s.likes.LikeInfo(r.Context(), ids, p.UserID)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].LikeCount = counts[items[i].ID]
		items[i].Liked = mine[items[i].ID]
	}
	return nil
}

func (s *Server) likePost(w http.ResponseWriter, r *http.Request) { s.setLike(w, r, true) }

func (s *Server) unlikePost(w http.ResponseWriter, r *http.Request) { s.setLike(w, r, false) }

// setLike is idempotent in both directions, so client retries are safe.
func (s *Server) setLike(w http.ResponseWriter, r *http.Request, like bool) {
	if s.likes == nil {
		writeError(w, r, http.StatusNotImplemented, "not_implemented", "likes are not enabled")
		return
	}
	id, ok := s.postID(w, r)
	if !ok {
		return
	}
	p, _ := auth.FromContext(r.Context())
	var err error
	if like {
		err = s.likes.Like(r.Context(), p.UserID, id)
	} else {
		err = s.likes.Unlike(r.Context(), p.UserID, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "post not found")
		return
	}
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forYou is the personalised feed. If ranking is unavailable (no models, or
// Elasticsearch down) clients should fall back to the chronological GET /posts.
func (s *Server) forYou(w http.ResponseWriter, r *http.Request) {
	if s.feed == nil {
		writeError(w, r, http.StatusServiceUnavailable, "feed_unavailable", "personalised feed is not enabled")
		return
	}
	limit, ok := parseLimit(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "limit must be 1-100")
		return
	}
	p, _ := auth.FromContext(r.Context())
	items, err := s.feed.ForYou(r.Context(), p.UserID, p.Username, limit)
	if errors.Is(err, feed.ErrBusy) {
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusServiceUnavailable, "feed_busy",
			"recommendations are busy; the latest feed still works")
		return
	}
	if errors.Is(err, search.ErrUnavailable) {
		writeError(w, r, http.StatusServiceUnavailable, "feed_unavailable",
			"personalised feed is temporarily unavailable; the latest feed still works")
		return
	}
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	type rankedPost struct {
		postResponse
		Ranking feed.Item `json:"ranking"`
	}
	out := struct {
		Items []rankedPost `json:"items"`
	}{Items: make([]rankedPost, len(items))}
	resp := make([]postResponse, len(items))
	for i, it := range items {
		resp[i] = docToResponse(it.Doc)
	}
	if err := s.attachLikes(r, resp); err != nil {
		internalError(w, r, s.log, err)
		return
	}
	for i := range items {
		out.Items[i] = rankedPost{postResponse: resp[i], Ranking: items[i]}
	}
	writeJSON(w, http.StatusOK, out)
}

func encodePageCursor(c store.PageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePageCursor(s string) (store.PageCursor, error) {
	var c store.PageCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.ID == uuid.Nil || c.CreatedAt.IsZero() {
		return c, errors.New("incomplete cursor")
	}
	return c, nil
}
