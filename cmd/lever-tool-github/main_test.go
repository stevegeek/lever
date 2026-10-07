package main

import (
	"errors"
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
