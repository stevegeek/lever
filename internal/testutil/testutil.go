// Package testutil holds the stdlib-only assertion helpers test packages
// across the module share. It must stay free of module and third-party
// imports so every package can use it without widening its test build.
package testutil

import (
	"errors"
	"strings"
	"testing"
)

// WantErrIs fails t unless errors.Is(err, target).
func WantErrIs(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error %v does not wrap %v", err, target)
	}
}

// WantErrContaining fails t unless err is non-nil and its message contains
// every substr. Use it only where the wording is the contract (user-facing
// hints); prefer WantErrIs when a sentinel exists.
func WantErrContaining(t testing.TB, err error, substrs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want one containing %q", substrs)
		return
	}
	for _, s := range substrs {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error %q does not contain %q", err, s)
			return
		}
	}
}
