package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreateIdempotent(t *testing.T) {
	const constraint = "widgets_owner_idempotency"
	key := "key"
	duplicate := &pgconn.PgError{Code: "23505", ConstraintName: constraint}

	t.Run("insert wins", func(t *testing.T) {
		lookupCalled := false
		row, replayed, err := createIdempotent(&key, constraint,
			func() (string, error) { lookupCalled = true; return "", nil },
			func() (string, error) { return "new", nil })
		if err != nil || replayed || row != "new" || lookupCalled {
			t.Fatalf("createIdempotent = (%q, %v, %v), lookupCalled=%v", row, replayed, err, lookupCalled)
		}
	})

	t.Run("concurrent winner replays", func(t *testing.T) {
		row, replayed, err := createIdempotent(&key, constraint,
			func() (string, error) { return "winner", nil },
			func() (string, error) { return "", duplicate })
		if err != nil || !replayed || row != "winner" {
			t.Fatalf("createIdempotent = (%q, %v, %v), want (%q, true, nil)", row, replayed, err, "winner")
		}
	})

	t.Run("other error passes through", func(t *testing.T) {
		boom := errors.New("boom")
		_, replayed, err := createIdempotent(&key, constraint,
			func() (string, error) { t.Fatal("lookup must not run"); return "", nil },
			func() (string, error) { return "", boom })
		if !errors.Is(err, boom) || replayed {
			t.Fatalf("createIdempotent err = %v, replayed=%v", err, replayed)
		}
	})

	t.Run("nil key never replays", func(t *testing.T) {
		row, replayed, err := createIdempotent[string](nil, constraint,
			func() (string, error) { t.Fatal("lookup must not run"); return "", nil },
			func() (string, error) { return "new", nil })
		if err != nil || replayed || row != "new" {
			t.Fatalf("createIdempotent = (%q, %v, %v)", row, replayed, err)
		}
	})
}

func TestReplayOr(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	key := "key"

	t.Run("no key continues", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handled := replayOr(s, rec, "lookup", nil,
			func() (string, error) { return "", nil },
			func(string) { t.Fatal("write must not run") })
		if handled {
			t.Fatal("replayOr handled a request without a key")
		}
	})

	t.Run("replay writes the answer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handled := replayOr(s, rec, "lookup", &key,
			func() (string, error) { return "row", nil },
			func(v string) { writeJSON(rec, http.StatusOK, v) })
		if !handled || rec.Code != http.StatusOK {
			t.Fatalf("replayOr handled=%v status=%d", handled, rec.Code)
		}
	})

	t.Run("lookup error writes internal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handled := replayOr(s, rec, "lookup", &key,
			func() (string, error) { return "", errors.New("boom") },
			func(string) { t.Fatal("write must not run") })
		if !handled || rec.Code != http.StatusInternalServerError {
			t.Fatalf("replayOr handled=%v status=%d", handled, rec.Code)
		}
	})
}
