package config

import (
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// missingBinary is a command name that is on no PATH, for host-check tests.
const missingBinary = "definitely-not-on-path-xyz"

// Fragments of validation messages that more than one test pins. The wording
// is user-facing contract, so a change lands here once.
const (
	msgUnknownBackend = "unknown backend"
	msgOperatorPrefix = "config: operator:"
)

// rejectNoHost loads body through LoadNoHostChecks and requires a rejection
// whose message contains every substr.
func rejectNoHost(t *testing.T, body string, substrs ...string) {
	t.Helper()
	_, err := LoadNoHostChecks(writeConfig(t, body))
	testutil.WantErrContaining(t, err, substrs...)
}
