package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// loadVisible resolves one owned row and writes the same 404 for a missing row
// and a row the caller may not see (api.md §1). The getter returns the row's
// owner alongside the row so a two-step lookup (an Exec's Sandbox) stays
// inside the closure.
func loadVisible[T any](s *Server, w http.ResponseWriter, ctx context.Context, p *principal,
	id uuid.UUID, noun string,
	get func(context.Context, uuid.UUID) (T, uuid.UUID, error),
) (T, bool) {
	row, owner, err := get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ownerOrAdmin(p, owner)) {
		var zero T
		writeProblem(w, "not_found", "no such "+noun)
		return zero, false
	}
	if err != nil {
		var zero T
		s.internal(w, "get "+noun, err)
		return zero, false
	}
	return row, true
}

// ownerOrAdmin is the visibility rule for every owned resource.
func ownerOrAdmin(p *principal, owner uuid.UUID) bool {
	return p.isAdmin() || p.user.ID == owner
}

// readPage parses limit and cursor, writes the standard 400s, fetches one row
// past the page (the caller receives limit+1 and must pass it to the query),
// and returns the trimmed page with its next cursor.
func readPage[T any, C any](s *Server, w http.ResponseWriter, r *http.Request, what string,
	parseCursor func(*http.Request) (C, bool),
	fetch func(ctx context.Context, limit int32, after C) ([]T, error),
	cursor func(T) string,
) ([]T, *string, bool) {
	limit, ok := parseLimit(r)
	if !ok {
		writeProblem(w, "invalid_request", "limit must be between 1 and 200")
		return nil, nil, false
	}
	after, ok := parseCursor(r)
	if !ok {
		writeProblem(w, "invalid_request", "invalid cursor")
		return nil, nil, false
	}
	rows, err := fetch(r.Context(), limit+1, after)
	if err != nil {
		s.internal(w, what, err)
		return nil, nil, false
	}
	rows, next := trimPage(rows, limit, cursor)
	return rows, next, true
}

// createIdempotent inserts a row that carries an Idempotency Key. A concurrent
// request may win the unique index first; its row is then returned with
// replayed=true (ADR-0017). A nil key never replays.
func createIdempotent[T any](key *string, constraint string,
	lookup func() (T, error), insert func() (T, error)) (T, bool, error) {
	row, err := insert()
	if err == nil {
		return row, false, nil
	}
	if existing, replayed := replayConflict(key, err, constraint, lookup); replayed {
		return existing, true, nil
	}
	var zero T
	return zero, false, err
}

// replayOr answers an Idempotency-Key replay before the request body is read:
// ADR-0017 says a repeat returns the existing resource and the body is not
// compared. It reports whether it wrote the response.
func replayOr[T any](s *Server, w http.ResponseWriter, what string, key *string,
	lookup func() (T, error), write func(T)) bool {
	existing, replayed, err := replayExisting(key, lookup)
	if err != nil {
		s.internal(w, what, err)
		return true
	}
	if !replayed {
		return false
	}
	write(existing)
	return true
}
