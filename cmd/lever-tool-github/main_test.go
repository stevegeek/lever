package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/captool"
)

func TestPushBackstop(t *testing.T) {
	b := pushBackstop(map[string]bool{"stevegeek/lever": true}, "agent/")
	ok := map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "bundle": "x.bundle"}
	if err := b(captool.ValidatedContext{Operation: "push"}, ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{
		{"repo": "stevegeek/lever", "branch": "main", "bundle": "x.bundle"},
		{"repo": "x/y", "branch": "agent/x", "bundle": "x.bundle"},
		{"repo": "stevegeek/lever", "branch": "agent/x", "bundle": "../x.bundle"},
	} {
		if err := b(captool.ValidatedContext{Operation: "push"}, bad); err == nil {
			t.Errorf("backstop must refuse %v", bad)
		}
	}
	if err := b(captool.ValidatedContext{Operation: "delete"}, ok); err == nil {
		t.Error("backstop must refuse a non-push operation")
	}
}

func TestAsResult(t *testing.T) {
	v, err := asResult(nil, errors.New("branch moved"))
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["ok"] != false || m["error"] != "branch moved" {
		t.Fatalf("got %v %v", v, err)
	}
	v, err = asResult("x", nil)
	if err != nil || v != "x" {
		t.Fatalf("got %v %v", v, err)
	}
}

func TestParseFlagsRequired(t *testing.T) {
	_, err := parseFlags([]string{"-backend", "127.0.0.1:0", "-admin", "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "-tree") {
		t.Fatalf("want a missing -tree error, got %v", err)
	}
	_, err = parseFlags([]string{"-tree", "rel/path", "-state", "/s", "-app-id", "1", "-installation-id", "2", "-app-key", "/k", "-repos", "a/b"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want an absolute-path error, got %v", err)
	}
}

func TestParseFlagsRefusesStateInsideTree(t *testing.T) {
	tree := t.TempDir()
	base := []string{"-app-id", "1", "-installation-id", "2", "-app-key", "/k", "-repos", "a/b"}
	for _, state := range []string{filepath.Join(tree, "st"), filepath.Join(tree, "a", "b", "st"), tree} {
		_, err := parseFlags(append([]string{"-tree", tree, "-state", state}, base...))
		if err == nil || !strings.Contains(err.Error(), "inside") {
			t.Errorf("state %s: want an inside-tree refusal, got %v", state, err)
		}
	}
	// A symlinked path to the tree is the same tree.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(append([]string{"-tree", tree, "-state", filepath.Join(link, "st")}, base...)); err == nil {
		t.Error("state under a symlink to the tree must be refused")
	}
	if _, err := parseFlags(append([]string{"-tree", tree, "-state", filepath.Join(t.TempDir(), "st")}, base...)); err != nil {
		t.Errorf("separate state must be accepted: %v", err)
	}
}

func TestParseFlagsRefusesAppKeyInsideTree(t *testing.T) {
	tree := t.TempDir()
	base := []string{"-tree", tree, "-state", filepath.Join(t.TempDir(), "st"), "-app-id", "1", "-installation-id", "2", "-repos", "a/b"}
	key := filepath.Join(tree, "secrets", "app.pem")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parseFlags(append(base, "-app-key", key))
	if err == nil || !strings.Contains(err.Error(), "-app-key") || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("want an -app-key inside-tree refusal, got %v", err)
	}
	// A symlink outside the tree that points at a key inside it is the same file.
	link := filepath.Join(t.TempDir(), "app.pem")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(append(base, "-app-key", link)); err == nil {
		t.Error("a key reached through a symlink into the tree must be refused")
	}
	if _, err := parseFlags(append(base, "-app-key", filepath.Join(t.TempDir(), "app.pem"))); err != nil {
		t.Errorf("a key outside the tree must be accepted: %v", err)
	}
}
