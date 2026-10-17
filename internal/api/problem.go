package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// The error catalog is closed (api.md §2). A new situation needs a spec change,
// not a new code.
var problemStatus = map[string]int{
	"invalid_request":     http.StatusBadRequest,
	"unauthenticated":     http.StatusUnauthorized,
	"permission_denied":   http.StatusForbidden,
	"not_found":           http.StatusNotFound,
	"conflict":            http.StatusConflict,
	"precondition_failed": http.StatusPreconditionFailed,
	"payload_too_large":   http.StatusRequestEntityTooLarge,
	"unsupported":         http.StatusUnprocessableEntity,
	"quota_exceeded":      http.StatusTooManyRequests,
	"rate_limited":        http.StatusTooManyRequests,
	"internal":            http.StatusInternalServerError,
	"unavailable":         http.StatusServiceUnavailable,
}

var problemTitle = map[string]string{
	"invalid_request":     "Invalid request",
	"unauthenticated":     "Unauthenticated",
	"permission_denied":   "Permission denied",
	"not_found":           "Not found",
	"conflict":            "Conflict",
	"precondition_failed": "Precondition failed",
	"payload_too_large":   "Payload too large",
	"unsupported":         "Unsupported",
	"quota_exceeded":      "Quota exceeded",
	"rate_limited":        "Rate limited",
	"internal":            "Internal error",
	"unavailable":         "Unavailable",
}

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

func writeProblem(w http.ResponseWriter, code, detail string) {
	status, ok := problemStatus[code]
	if !ok {
		status, code = http.StatusInternalServerError, "internal"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemBody{
		Type:   "about:blank",
		Title:  problemTitle[code],
		Status: status,
		Detail: detail,
		Code:   code,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const maxJSONBody = 1 << 20 // 1 MiB

// decodeJSON rejects unknown fields and trailing data so a typo in a request
// fails loudly instead of being silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeProblem(w, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, "invalid_request", "body must contain exactly one JSON object")
		return false
	}
	return true
}
