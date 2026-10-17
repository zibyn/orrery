package api

import (
	"errors"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTrimPage(t *testing.T) {
	id := func(n int) string { return strconv.Itoa(n) }
	rows := []int{1, 2, 3, 4}

	page, next := trimPage(rows, 3, id)
	if len(page) != 3 || next == nil || *next != "3" {
		t.Fatalf("trimPage = %v, %v; want [1 2 3], 3", page, next)
	}
	page, next = trimPage(rows[:3], 3, id)
	if len(page) != 3 || next != nil {
		t.Fatalf("exact page = %v, %v; want [1 2 3], nil", page, next)
	}
}

func TestReplayExisting(t *testing.T) {
	key := "k"
	called := false
	lookup := func() (int, error) { called = true; return 7, nil }

	if _, replayed, _ := replayExisting(nil, lookup); replayed || called {
		t.Fatal("nil key must not look up")
	}
	if row, replayed, err := replayExisting(&key, lookup); err != nil || !replayed || row != 7 {
		t.Fatalf("replayExisting = %d, %v, %v; want 7, true, nil", row, replayed, err)
	}
	if _, replayed, err := replayExisting(&key, func() (int, error) { return 0, pgx.ErrNoRows }); replayed || err != nil {
		t.Fatalf("missing row = %v, %v; want false, nil", replayed, err)
	}
	if _, replayed, err := replayExisting(&key, func() (int, error) { return 0, errors.New("boom") }); replayed || err == nil {
		t.Fatalf("lookup error = %v, %v; want false, error", replayed, err)
	}
}

func TestReplayConflict(t *testing.T) {
	key := "k"
	duplicate := &pgconn.PgError{Code: "23505", ConstraintName: "things_idempotency"}
	lookup := func() (int, error) { return 7, nil }

	if row, replayed := replayConflict(&key, duplicate, "things_idempotency", lookup); !replayed || row != 7 {
		t.Fatalf("conflict = %d, %v; want 7, true", row, replayed)
	}
	if _, replayed := replayConflict(&key, duplicate, "other_constraint", lookup); replayed {
		t.Fatal("a different unique constraint must not replay")
	}
	if _, replayed := replayConflict(nil, duplicate, "things_idempotency", lookup); replayed {
		t.Fatal("a nil key must not replay")
	}
}
