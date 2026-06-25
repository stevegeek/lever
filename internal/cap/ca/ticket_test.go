package ca

import (
	"testing"
	"time"
)

func TestTicketRedeemsOnceForItsGrove(t *testing.T) {
	s := NewTicketStore()
	tk, err := s.Issue("scratch", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tk == "" {
		t.Fatal("empty ticket")
	}
	if err := s.Redeem(tk, "scratch", time.Now()); err != nil {
		t.Fatalf("first redeem should succeed: %v", err)
	}
	if err := s.Redeem(tk, "scratch", time.Now()); err == nil {
		t.Fatal("second redeem must fail (single-use)")
	}
}

func TestTicketRejectsWrongGrove(t *testing.T) {
	s := NewTicketStore()
	tk, _ := s.Issue("scratch", time.Hour)
	if err := s.Redeem(tk, "other", time.Now()); err == nil {
		t.Fatal("redeem must fail for a different grove")
	}
	// And the ticket must NOT be burned by a failed mismatched attempt.
	if err := s.Redeem(tk, "scratch", time.Now()); err != nil {
		t.Fatalf("correct grove should still redeem: %v", err)
	}
}

func TestTicketRejectsExpired(t *testing.T) {
	s := NewTicketStore()
	tk, _ := s.Issue("scratch", time.Hour)
	if err := s.Redeem(tk, "scratch", time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired ticket must fail")
	}
}

func TestTicketRejectsUnknown(t *testing.T) {
	s := NewTicketStore()
	if err := s.Redeem("nope", "scratch", time.Now()); err == nil {
		t.Fatal("unknown ticket must fail")
	}
}

func TestIssuePrunesExpiredTickets(t *testing.T) {
	s := NewTicketStore()
	if _, err := s.Issue("ghost", time.Nanosecond); err != nil { // expires ~immediately
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := s.Issue("scratch", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n := len(s.tickets)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected the expired ticket pruned on Issue, got %d tickets", n)
	}
}
