package lockout

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newStore(t *testing.T) (Store, *time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/l.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE login_failures (account_key TEXT, kind TEXT, failed_at_ms INTEGER)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	return Store{DB: db, Now: func() time.Time { return now }}, &now
}

func fail(t *testing.T, s Store, key string) {
	t.Helper()
	if err := s.Record(context.Background(), key, "password"); err != nil {
		t.Fatal(err)
	}
}

func retryOf(t *testing.T, err error) int {
	t.Helper()
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("want *LockedError, got %v", err)
	}
	if le.Status() != 429 || le.Code() != "LOGIN_LOCKED" {
		t.Fatalf("status/code %d %s", le.Status(), le.Code())
	}
	return le.RetryAfter
}

func TestFirstFourFailuresAreFree(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := s.Check(ctx, "k"); err != nil {
			t.Fatalf("try %d refused: %v", i+1, err)
		}
		fail(t, s, "k")
	}
	if err := s.Check(ctx, "k"); err != nil {
		t.Fatalf("5th try is still allowed: %v", err)
	}
}

func TestBackoffDoublesFromTheFifthFailure(t *testing.T) {
	s, now := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		fail(t, s, "k")
	}
	if got := retryOf(t, s.Check(ctx, "k")); got != 2 {
		t.Fatalf("after 5 failures wait 2s, got %d", got)
	}
	*now = now.Add(2 * time.Second)
	if err := s.Check(ctx, "k"); err != nil {
		t.Fatalf("allowed once the wait passed: %v", err)
	}
	fail(t, s, "k") // 6th
	if got := retryOf(t, s.Check(ctx, "k")); got != 4 {
		t.Fatalf("after 6 failures wait 4s, got %d", got)
	}
}

func TestTenthFailureLocksUntilTheWindowMoves(t *testing.T) {
	s, now := newStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		fail(t, s, "k")
		*now = now.Add(time.Second)
	}
	got := retryOf(t, s.Check(ctx, "k"))
	if got < 880 || got > 900 {
		t.Fatalf("locked for ~15 min, got %ds", got)
	}
	*now = now.Add(16 * time.Minute)
	if err := s.Check(ctx, "k"); err != nil {
		t.Fatalf("lock must end after the window: %v", err)
	}
}

func TestPerKeyAndClear(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		fail(t, s, ConfirmKey("1"))
	}
	if s.Check(ctx, ConfirmKey("2")) != nil {
		t.Fatal("another account is unaffected")
	}
	s.Record(ctx, ConfirmKey("1"), "second_factor")
	if err := s.Clear(ctx, ConfirmKey("1"), "password"); err != nil {
		t.Fatal(err)
	}
	if s.Check(ctx, ConfirmKey("1")) != nil {
		t.Fatal("clearing the password failures leaves one second-factor failure only")
	}
	s.Clear(ctx, ConfirmKey("1"), "")
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM login_failures`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left", n)
	}
}

func TestMessageWording(t *testing.T) {
	if m := (&LockedError{RetryAfter: 30}).Error(); !strings.HasSuffix(m, "Try again in 30 seconds.") {
		t.Fatal(m)
	}
	if m := (&LockedError{RetryAfter: 120}).Error(); !strings.HasSuffix(m, "Try again in 2 minutes.") {
		t.Fatal(m)
	}
}

func TestParallelAttemptsAreCountedBeforeTheyPass(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	results := make(chan error, 20)
	for range 20 {
		go func() { results <- s.Attempt(ctx, "k", "password") }()
	}
	passed := 0
	for range 20 {
		err := <-results
		if err == nil {
			passed++
			continue
		}
		retryOf(t, err)
	}
	if passed != failuresDelay {
		t.Fatalf("%d parallel tries passed the check, want %d", passed, failuresDelay)
	}
}
