package config

import (
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

const missingBinary = "definitely-not-on-path-xyz"

// Fragments of validation messages that more than one test pins. The wording
// is user-facing contract, so a change lands here once.
const (
	msgUnknownBackend = "unknown backend"
	msgOperatorPrefix = "config: operator:"
)

func rejectNoHost(t *testing.T, body string, substrs ...string) {
	t.Helper()
	_, err := LoadNoHostChecks(writeConfig(t, body))
	testutil.WantErrContaining(t, err, substrs...)
}
