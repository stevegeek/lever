package main

import (
	"testing"

	"github.com/stevegeek/lever/captool"
)

func TestReadBackstopRejectsForbiddenTableAndWrites(t *testing.T) {
	if err := readBackstop(captool.ValidatedContext{Operation: "read"}, map[string]string{"table": "C"}); err == nil {
		t.Fatal("backstop must reject table C")
	}
	if err := readBackstop(captool.ValidatedContext{Operation: "read"}, map[string]string{"table": "A"}); err != nil {
		t.Fatalf("backstop must allow table A: %v", err)
	}
	if err := readBackstop(captool.ValidatedContext{Operation: "write"}, map[string]string{"table": "A"}); err == nil {
		t.Fatal("backstop must reject a non-read operation")
	}
}

func TestBuildServerDeclaresReadOperation(t *testing.T) {
	st := newTestStore(t)
	s, err := buildServer(st, "db", "127.0.0.1:0", "http://127.0.0.1:0")
	if err != nil || s == nil {
		t.Fatalf("buildServer: %v", err)
	}
}
