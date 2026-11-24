package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorBody is the single error shape every endpoint returns, so clients can
// branch on a stable machine-readable `code` instead of parsing messages.
//
//	{"error": {"code": "not_found", "message": "post not found"}, "request_id": "..."}
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = msg
	b.RequestID = requestIDFrom(r.Context())
	writeJSON(w, status, b)
}

// internalError logs the real cause and returns a generic message; internal
// details (SQL errors, hostnames) must not leak to clients.
func internalError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	log.ErrorContext(r.Context(), "internal error", "err", err, "request_id", requestIDFrom(r.Context()),
		"method", r.Method, "path", r.URL.Path)
	writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
}
