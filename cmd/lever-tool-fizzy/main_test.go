package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	_, err := parseFlags([]string{"-fizzy", "fizzy", "-token-file", "/t", "-account", "1", "-board", "03gyvmtu0lb2osl1x3h5hkeii", "-state", "/s"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative -fizzy must fail: %v", err)
	}
	_, err = parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "BAD BOARD", "-state", "/s"})
	if err == nil {
		t.Fatal("an invalid board id must fail")
	}
	_, err = parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "03gyvmtu0lb2osl1x3h5hkeii", "-state", "/s", "-prefix", " "})
	if err == nil {
		t.Fatal("an empty prefix must fail")
	}
	// the empty board must fail (checkCard would otherwise fail open)
	_, err = parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "", "-state", "/s"})
	if err == nil {
		t.Fatal("an empty board must fail")
	}
	o, err := parseFlags([]string{"-fizzy", "/f", "-token-file", "/t", "-account", "1", "-board", "03gyvmtu0lb2osl1x3h5hkeii", "-state", "/s"})
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
