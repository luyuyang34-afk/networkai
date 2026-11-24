package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"networkai/internal/auth"
	"networkai/internal/store"
)

// usernameRe must match the CHECK constraint on users.username.
//
// The original code used `^[a-z0-9]$` (exactly ONE character) and rejected
// names that MATCHED it, so it only blocked single-character names and
// accepted anything else, including spaces and symbols.
var usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

const minPasswordLen = 8

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decodeCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	var c credentials
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "body must be JSON with username and password")
		return c, false
	}
	return c, true
}

func validateCredentials(c credentials) string {
	switch {
	case !usernameRe.MatchString(c.Username):
		return "username must be 3-20 characters of a-z, 0-9 or _"
	case len(c.Password) < minPasswordLen:
		return "password must be at least 8 characters"
	case len(c.Password) > auth.MaxPasswordBytes:
		return "password must be at most 72 bytes"
	}
	return ""
}

type userResponse struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	if msg := validateCredentials(c); msg != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_input", msg)
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	u, err := s.users.CreateUser(r.Context(), c.Username, hash)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, r, http.StatusConflict, "username_taken", "username already exists")
		return
	}
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusCreated, userResponse{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	u, err := s.users.GetUserByUsername(r.Context(), c.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, r, s.log, err)
		return
	}
	// u.PasswordHash is "" for unknown users; CheckPassword still spends the
	// same time hashing, and the response is identical either way.
	if !auth.CheckPassword(u.PasswordHash, c.Password) {
		writeError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	token, exp, err := s.tokens.Issue(auth.Principal{UserID: u.ID, Username: u.Username})
	if err != nil {
		internalError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": exp.UTC()})
}
