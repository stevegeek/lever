package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/fizzytool"
)

func TestLoadTokenMode(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "tok")
	os.WriteFile(p, []byte("fz_abc\n"), 0o600)
	if tok, err := loadToken(p); err != nil || tok != "fz_abc" {
		t.Fatalf("%q %v", tok, err)
	}
	os.Chmod(p, 0o644)
	if _, err := loadToken(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("0644 must be refused: %v", err)
	}
}

func TestParseFlagsFizzy(t *testing.T) {
	if _, err := parseFlags([]string{"-fizzy", "/usr/local/bin/fizzy"}); err == nil {
		t.Fatal("missing flags must fail")
	}
	_, err := parseFlags([]string{"-fizzy", "fizzy", "-token-file", "/t", "-account", "1", "-board", "0testboard00000000000001", "-state", "/s"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative -fizzy must fail: %v", err)
	}
	_, err = parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "BAD BOARD", "-state", "/s"})
	if err == nil {
		t.Fatal("an invalid board id must fail")
	}
	_, err = parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "0testboard00000000000001", "-state", "/s", "-prefix", " "})
	if err == nil {
		t.Fatal("an empty prefix must fail")
	}
	// the empty board must fail (checkCard would otherwise fail open)
	_, err = parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "", "-state", "/s"})
	if err == nil {
		t.Fatal("an empty board must fail")
	}
	o, err := parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "0testboard00000000000001", "-state", "/s"})
	if err != nil || o.prefix != "[lever-dev agent] " || o.backend != "127.0.0.1:3211" || o.name != "fizzy" {
		t.Fatalf("defaults: %+v %v", o, err)
	}
}

func TestAsResultFizzy(t *testing.T) {
	v, err := asResult(nil, fmt.Errorf("card 99: card is not on the Lever board"))
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["ok"] != false || !strings.Contains(m["error"].(string), "Lever board") {
		t.Fatalf("got %v %v", v, err)
	}
}

func opNames(ops []captool.Operation) []string {
	var n []string
	for _, o := range ops {
		n = append(n, o.Name)
	}
	return n
}

// TestOpsFlag: unset -ops keeps the six operations and no close_card;
// close_card is registered only when -ops lists it.
func TestOpsFlag(t *testing.T) {
	base := []string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "0testboard00000000000001", "-state", "/s"}
	o, err := parseFlags(base)
	if err != nil {
		t.Fatal(err)
	}
	got := opNames(operations(context.Background(), o, &fizzytool.Tool{}))
	if want := []string{"list_cards", "show_card", "list_comments", "comment", "move_card", "create_card"}; !slices.Equal(got, want) {
		t.Fatalf("default ops: got %v, want %v", got, want)
	}
	o, err = parseFlags(append(slices.Clone(base), "-ops", " close_card,show_card,close_card"))
	if err != nil {
		t.Fatal(err)
	}
	if got := opNames(operations(context.Background(), o, &fizzytool.Tool{})); !slices.Equal(got, []string{"show_card", "close_card"}) {
		t.Fatalf("listed ops: got %v", got)
	}
	for _, bad := range []string{"close", "delete_card", "", "show_card,"} {
		if _, err := parseFlags(append(slices.Clone(base), "-ops", bad)); err == nil || !strings.Contains(err.Error(), "-ops") {
			t.Errorf("-ops %q: want a refusal, got %v", bad, err)
		}
	}
}
