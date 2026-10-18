package api

import "net/http"

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
