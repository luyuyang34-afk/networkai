package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"networkai/internal/auth"
	"networkai/internal/metrics"
)

type ridKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}

// requestID tags each request with an id (reusing a client-supplied
// X-Request-ID if present) so a log line can be matched to an error response.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			b := make([]byte, 8)
			rand.Read(b) //nolint:errcheck // crypto/rand never fails on supported platforms
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ridKey{}, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// accessLog emits one structured log line per request and records metrics.
func accessLog(log *slog.Logger, m *metrics.Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		elapsed := time.Since(start)
		// ServeMux sets r.Pattern on the request it routed, which is this
		// same *http.Request, so the route template is available here.
		if m != nil && r.Pattern != "GET /metrics" {
			m.ObserveHTTP(r.Pattern, rec.status, elapsed)
		}
		log.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "route", r.Pattern, "duration_ms", float64(elapsed.Microseconds())/1000,
			"request_id", requestIDFrom(r.Context()))
	})
}

// recoverer turns a handler panic into a 500 instead of killing the
// connection (net/http would otherwise just drop it).
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic", "value", v, "request_id", requestIDFrom(r.Context()))
				writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// cors allows only configured origins (the original allowed "*").
func cors(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(origins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Expose-Headers", "X-Request-ID")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// optionalAuth attaches the principal when a valid token is present and
// otherwise serves the request anonymously (used to personalise read
// responses, e.g. "liked by me").
func (s *Server) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := auth.BearerToken(r); ok {
			if p, err := s.tokens.Parse(tok); err == nil {
				r = r.WithContext(auth.WithPrincipal(r.Context(), p))
			}
		}
		next(w, r)
	}
}

// requireAuth rejects requests without a valid bearer token and puts the
// authenticated principal into the request context.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := auth.BearerToken(r)
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "missing bearer token")
			return
		}
		p, err := s.tokens.Parse(tok)
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "invalid or expired token")
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}
