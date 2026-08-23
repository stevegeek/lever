// Package clitest holds the cobra test helpers the host and manager CLI
// packages share.
package clitest

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Exec runs cmd with argv, capturing stdout and stderr into one buffer, and
// returns the combined output with Execute's error.
func Exec(t *testing.T, cmd *cobra.Command, argv ...string) (string, error) {
	t.Helper()
	cmd.SetArgs(argv)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

// Names is the set of root's direct subcommand names.
func Names(root *cobra.Command) map[string]bool {
	m := map[string]bool{}
	for _, c := range root.Commands() {
		m[c.Name()] = true
	}
	return m
}

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
