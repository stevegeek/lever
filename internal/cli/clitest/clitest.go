// Package clitest holds the cobra test helpers the host and manager CLI
// packages share.
package clitest

import (
	"bytes"
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
