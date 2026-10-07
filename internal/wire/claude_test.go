package wire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClaudeValidate(t *testing.T) {
	cases := []struct {
		c    *Claude
		want error
	}{
		{nil, nil},
		{&Claude{}, nil},
		{&Claude{AutoCompactWindow: MinAutoCompactWindow}, nil},
		{&Claude{AutoCompactWindow: MaxAutoCompactWindow}, nil},
		{&Claude{AutoCompactWindow: MinAutoCompactWindow - 1}, ErrAutoCompactWindowRange},
		{&Claude{AutoCompactWindow: MaxAutoCompactWindow + 1}, ErrAutoCompactWindowRange},
		{&Claude{AutoCompactWindow: -5}, ErrAutoCompactWindowRange},
		{&Claude{AfterCompactNote: "Re-read STATE.md — then go on. ✓"}, nil},
		{&Claude{AfterCompactNote: strings.Repeat("a", MaxAfterCompactNoteBytes)}, nil},
		{&Claude{AfterCompactNote: strings.Repeat("a", MaxAfterCompactNoteBytes+1)}, ErrAfterCompactNoteLong},
		{&Claude{AfterCompactNote: "a\nb"}, ErrAfterCompactNoteText},
		{&Claude{AfterCompactNote: "a\rb"}, ErrAfterCompactNoteText},
		{&Claude{AfterCompactNote: "a\x1b[2Jb"}, ErrAfterCompactNoteText},
		{&Claude{AfterCompactNote: "a b"}, ErrAfterCompactNoteText},
		{&Claude{AfterCompactNote: "a‮b"}, ErrAfterCompactNoteText},
		{&Claude{AfterCompactNote: "a\xffb"}, ErrAfterCompactNoteText},
	}
	for _, c := range cases {
		if got := c.c.Validate(); !errors.Is(got, c.want) {
			t.Errorf("Validate(%+v) = %v, want %v", c.c, got, c.want)
		}
	}
}

func TestClaudeIsZero(t *testing.T) {
	if !(*Claude)(nil).IsZero() || !(&Claude{}).IsZero() {
		t.Fatal("nil and empty must be zero")
	}
	if (&Claude{AutoCompactWindow: MinAutoCompactWindow}).IsZero() {
		t.Fatal("a set window is not zero")
	}
}

// An envelope without a claude block marshals as before (no "claude" key),
// so an agent image that predates the block reads the same bytes.
func TestBootstrapOmitsEmptyClaude(t *testing.T) {
	b, err := json.Marshal(Bootstrap{Ticket: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "claude") {
		t.Fatalf("envelope %s carries a claude key with nothing configured", b)
	}
	b, err = json.Marshal(Bootstrap{Ticket: "t", Claude: &Claude{AutoCompactWindow: 400000}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"claude":{"auto_compact_window":400000}`) {
		t.Fatalf("envelope %s lacks the claude block", b)
	}
}
