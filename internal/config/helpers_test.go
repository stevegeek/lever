package config

import (
	"strings"
	"testing"
)

// missingBinary is a command name that is on no PATH, for host-check tests.
const missingBinary = "definitely-not-on-path-xyz"

// Fragments of validation messages that more than one test pins. The wording
// is user-facing contract, so a change lands here once.
const (
	msgUnknownBackend = "unknown backend"
	msgOperatorPrefix = "config: operator:"
)

// wantErrContaining fails the test unless err is non-nil and its message
// contains every substr. Validation wording is part of lever's user-facing
// contract, so tests pin the fragments an operator would grep for.
func wantErrContaining(t testing.TB, err error, substrs ...string) {
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

// rejectNoHost loads body through LoadNoHostChecks and requires a rejection
// whose message contains every substr.
func rejectNoHost(t *testing.T, body string, substrs ...string) {
	t.Helper()
	_, err := LoadNoHostChecks(writeConfig(t, body))
	wantErrContaining(t, err, substrs...)
}
