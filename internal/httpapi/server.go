// Package httpapi is the REST layer: routing, validation, auth, and mapping
// domain errors to HTTP responses. Business data lives behind small
// interfaces so handlers can be unit-tested with fakes.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"networkai/internal/assist"
	"networkai/internal/auth"
	"networkai/internal/feed"
	"networkai/internal/media"
	"networkai/internal/metrics"
	"networkai/internal/outbox"
	"networkai/internal/search"
	"networkai/internal/store"

	"github.com/google/uuid"
)

type UserStore interface {
	CreateUser(ctx context.Context, username, passwordHash string) (store.User, error)
	GetUserByUsername(ctx context.Context, username string) (store.User, error)
}

type PostStore interface {
	CreatePost(ctx context.Context, np store.NewPost) (store.Post, error)
	UpdatePostMessage(ctx context.Context, id uuid.UUID, authorID int64, message string) (store.Post, error)
	DeletePost(ctx context.Context, id uuid.UUID, authorID int64) (store.Post, error)
	GetPost(ctx context.Context, id uuid.UUID) (store.Post, error)
	ListPosts(ctx context.Context, lp store.ListParams) ([]store.Post, *store.PageCursor, error)
}

// Likes is optional; without it posts report zero likes.
type Likes interface {
	Like(ctx context.Context, userID int64, postID uuid.UUID) error
	Unlike(ctx context.Context, userID int64, postID uuid.UUID) error
	LikeInfo(ctx context.Context, postIDs []string, userID int64) (map[string]int, map[string]bool, error)
}

// Assistant rewrites drafts with an LLM; optional.
type Assistant interface {
	Rewrite(ctx context.Context, userID int64, mode assist.Mode, draft string) (assist.Result, error)
}

// Feed is the personalised feed; optional (requires trained models).
type Feed interface {
	ForYou(ctx context.Context, userID int64, username string, limit int) ([]feed.Item, error)
}

type Searcher interface {
	Search(ctx context.Context, q search.Query) (search.Result, error)
}

// Readiness reports dependency health for /readyz.
type Readiness interface {
	Ping(ctx context.Context) error
	OutboxStats(ctx context.Context) (outbox.Stats, error)
}

type Deps struct {
	Users          UserStore
	Posts          PostStore
	Search         Searcher
	Media          media.Store
	Ready          Readiness
	Tokens         *auth.Tokens
	Log            *slog.Logger
	CORSOrigins    []string
	MaxUploadBytes int64
	// LocalMediaDir, if set, is served at /media/ (local media backend only).
	LocalMediaDir string
	// Metrics, if set, records request metrics and is served at /metrics.
	Metrics   *metrics.Registry
	Likes     Likes
	Feed      Feed
	Assistant Assistant
	Social    Social
	Timelines Timelines
}

type Server struct {
	users     UserStore
	posts     PostStore
	search    Searcher
	media     media.Store
	ready     Readiness
	tokens    *auth.Tokens
	log       *slog.Logger
	maxUpload int64
	metrics   *metrics.Registry
	likes     Likes
	feed      Feed
	assistant Assistant
	social    Social
	timelines Timelines
}

func New(d Deps) http.Handler {
	s := &Server{
		users: d.Users, posts: d.Posts, search: d.Search, media: d.Media, ready: d.Ready,
		tokens: d.Tokens, log: d.Log, maxUpload: d.MaxUploadBytes, metrics: d.Metrics,
		likes: d.Likes, feed: d.Feed, assistant: d.Assistant,
		social: d.Social, timelines: d.Timelines,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.maxUpload <= 0 {
		s.maxUpload = 50 << 20
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)

	mux.HandleFunc("POST /users", s.signup)
	mux.HandleFunc("POST /auth/login", s.login)

	mux.HandleFunc("GET /posts", s.optionalAuth(s.listPosts))
	mux.HandleFunc("GET /posts/search", s.optionalAuth(s.searchPosts))
	mux.HandleFunc("GET /posts/{id}", s.optionalAuth(s.getPost))
	mux.HandleFunc("POST /posts/{id}/like", s.requireAuth(s.likePost))
	mux.HandleFunc("DELETE /posts/{id}/like", s.requireAuth(s.unlikePost))
	mux.HandleFunc("GET /feed", s.requireAuth(s.forYou))
	mux.HandleFunc("POST /assist/rewrite", s.requireAuth(s.rewrite))
	mux.HandleFunc("GET /users/{username}", s.optionalAuth(s.getProfile))
	mux.HandleFunc("POST /users/{username}/follow", s.requireAuth(s.follow))
	mux.HandleFunc("DELETE /users/{username}/follow", s.requireAuth(s.unfollow))
	mux.HandleFunc("GET /timeline", s.requireAuth(s.homeTimeline))
	mux.HandleFunc("POST /posts", s.requireAuth(s.createPost))
	mux.HandleFunc("PATCH /posts/{id}", s.requireAuth(s.updatePost))
	mux.HandleFunc("DELETE /posts/{id}", s.requireAuth(s.deletePost))

	if d.Metrics != nil {
		// In production, expose this on an internal port or restrict it at
		// the load balancer; it is public here for local development.
		mux.Handle("GET /metrics", d.Metrics.Handler())
	}
	if d.LocalMediaDir != "" {
		mux.Handle("GET /media/", http.StripPrefix("/media/", http.FileServer(http.Dir(d.LocalMediaDir))))
	}

	// Outermost first: request id → recover → access log → CORS → routes.
	var h http.Handler = mux
	h = cors(d.CORSOrigins, h)
	h = accessLog(s.log, s.metrics, h)
	h = recoverer(s.log, h)
	h = requestID(h)
	return h
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is ready when PostgreSQL is reachable. Elasticsearch is deliberately
// NOT a readiness dependency: writes must keep working during a search outage
// (the outbox absorbs them), so taking the API out of rotation would turn a
// partial outage into a full one.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.ready == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if err := s.ready.Ping(r.Context()); err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	stats, err := s.ready.OutboxStats(r.Context())
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "outbox": stats})
}
