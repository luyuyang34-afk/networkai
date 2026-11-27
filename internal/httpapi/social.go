package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"networkai/internal/auth"
	"networkai/internal/store"
	"networkai/internal/timeline"
)

// Social is the follow graph; optional.
type Social interface {
	Follow(ctx context.Context, followerID int64, followee string) (int64, error)
	Unfollow(ctx context.Context, followerID int64, followee string) (int64, error)
	GetProfile(ctx context.Context, username string, viewerID int64) (store.Profile, error)
}

// Timelines serves the home timeline; optional (needs Redis).
type Timelines interface {
	Read(ctx context.Context, userID int64, before time.Time, limit int) (timeline.Page, error)
	Invalidate(ctx context.Context, userID int64) error
}

type profileResponse struct {
	Username       string `json:"username"`
	FollowerCount  int64  `json:"follower_count"`
	FollowingCount int64  `json:"following_count"`
	FollowedByMe   bool   `json:"followed_by_me"`
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	if s.social == nil {
		writeError(w, r, http.StatusNotImplemented, "not_implemented", "follows are not enabled")
		return
	}
	p, _ := auth.FromContext(r.Context())
	prof, err := s.social.GetProfile(r.Context(), r.PathValue("username"), p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, profileResponse{Username: prof.Username, FollowerCount: prof.FollowerCount,
		FollowingCount: prof.FollowingCount, FollowedByMe: prof.FollowedByMe})
}

func (s *Server) follow(w http.ResponseWriter, r *http.Request)   { s.setFollow(w, r, true) }
func (s *Server) unfollow(w http.ResponseWriter, r *http.Request) { s.setFollow(w, r, false) }

// setFollow is idempotent in both directions. After the graph changes the
// follower's cached timeline is dropped and rebuilt from PostgreSQL on the
// next read, so it immediately reflects the new set of followees.
func (s *Server) setFollow(w http.ResponseWriter, r *http.Request, follow bool) {
	if s.social == nil {
		writeError(w, r, http.StatusNotImplemented, "not_implemented", "follows are not enabled")
		return
	}
	p, _ := auth.FromContext(r.Context())
	var err error
	if follow {
		_, err = s.social.Follow(r.Context(), p.UserID, r.PathValue("username"))
	} else {
		_, err = s.social.Unfollow(r.Context(), p.UserID, r.PathValue("username"))
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "user not found")
		return
	case errors.Is(err, store.ErrSelfFollow):
		writeError(w, r, http.StatusBadRequest, "invalid_input", "you cannot follow yourself")
		return
	case err != nil:
		internalError(w, r, s.log, err)
		return
	}
	if s.timelines != nil {
		if err := s.timelines.Invalidate(r.Context(), p.UserID); err != nil {
			// The follow itself succeeded; a stale cached timeline only
			// delays the change until it expires, so log and carry on.
			s.log.WarnContext(r.Context(), "timeline invalidation failed", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// homeTimeline returns posts from accounts the user follows, newest first.
// cursor is the created_at (unix milliseconds) of the last post seen.
func (s *Server) homeTimeline(w http.ResponseWriter, r *http.Request) {
	if s.timelines == nil {
		writeError(w, r, http.StatusServiceUnavailable, "timeline_unavailable", "home timeline is not enabled")
		return
	}
	limit, ok := parseLimit(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "limit must be 1-100")
		return
	}
	var before time.Time
	if c := r.URL.Query().Get("cursor"); c != "" {
		ms, err := strconv.ParseInt(c, 10, 64)
		if err != nil || ms <= 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		before = time.UnixMilli(ms)
	}
	p, _ := auth.FromContext(r.Context())
	pg, err := s.timelines.Read(r.Context(), p.UserID, before, limit)
	if err != nil {
		s.log.WarnContext(r.Context(), "timeline read failed", "err", err)
		writeError(w, r, http.StatusServiceUnavailable, "timeline_unavailable",
			"home timeline is temporarily unavailable; the latest feed still works")
		return
	}
	resp := pageResponse{Items: make([]postResponse, len(pg.Posts))}
	for i, post := range pg.Posts {
		resp.Items[i] = toResponse(post)
	}
	if err := s.attachLikes(r, resp.Items); err != nil {
		internalError(w, r, s.log, err)
		return
	}
	if pg.NextCursor != nil {
		resp.NextCursor = strconv.FormatInt(pg.NextCursor.UnixMilli(), 10)
	}
	writeJSON(w, http.StatusOK, resp)
}
