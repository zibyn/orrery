package api

import (
	"testing"
	"time"
)

func TestLoginLimiterBlocksAfterTenFailures(t *testing.T) {
	l := newLoginLimiter()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	key := "alice\x00192.0.2.1"

	for i := 0; i < loginFailureLimit; i++ {
		if _, blocked := l.retryAfter(key, now); blocked {
			t.Fatalf("blocked before failure %d", i+1)
		}
		l.fail(key, now)
		now = now.Add(30 * time.Second)
	}
	retry, blocked := l.retryAfter(key, now)
	if !blocked {
		t.Fatal("not blocked after the failure limit")
	}
	if retry <= 0 || retry > loginFailureWindow {
		t.Fatalf("retry after = %v, want (0, %v]", retry, loginFailureWindow)
	}
}

func TestLoginLimiterSucceedsToReset(t *testing.T) {
	l := newLoginLimiter()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	key := "alice\x00192.0.2.1"

	for i := 0; i < loginFailureLimit; i++ {
		l.fail(key, now)
	}
	if _, blocked := l.retryAfter(key, now); !blocked {
		t.Fatal("not blocked after the failure limit")
	}
	l.succeed(key)
	if _, blocked := l.retryAfter(key, now); blocked {
		t.Fatal("still blocked after a successful login")
	}
}

func TestLoginLimiterWindowExpiresAndKeysAreIsolated(t *testing.T) {
	l := newLoginLimiter()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	alice, bob := "alice\x00192.0.2.1", "bob\x00192.0.2.1"

	for i := 0; i < loginFailureLimit; i++ {
		l.fail(alice, now)
	}
	if _, blocked := l.retryAfter(bob, now); blocked {
		t.Fatal("one username's failures blocked another")
	}
	if _, blocked := l.retryAfter(alice, now.Add(loginFailureWindow+time.Second)); blocked {
		t.Fatal("still blocked after the window expired")
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.1:54321", "192.0.2.1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"192.0.2.1", "192.0.2.1"},
	} {
		if got := splitClientIP(tc.remote); got != tc.want {
			t.Errorf("splitClientIP(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}
