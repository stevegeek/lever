package manager

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// execCmd runs cmd with argv, capturing stdout and stderr into one buffer,
// and returns the combined output with Execute's error.
func execCmd(t *testing.T, cmd *cobra.Command, argv ...string) (string, error) {
	t.Helper()
	cmd.SetArgs(argv)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}
