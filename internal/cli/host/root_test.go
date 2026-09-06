package host

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/cli/clitest"
)

func TestHostRootHasProvisioningOnly(t *testing.T) {
	n := clitest.Names(NewRoot())
	// msg is deliberately on BOTH roots: the host's is operator-authority,
	// fire-and-forget, no-broker-hop (attachTarget + scion.Client.Message
	// directly); the manager's is broker-routed send+list. Different trust
	// models, same verb name.
	for _, want := range []string{"up", "apply", "destroy", "stop", "doctor", "provision", "attach", "msg", "version"} {
		if !n[want] {
			t.Errorf("host root missing %q", want)
		}
	}
	for _, unwanted := range []string{"agent", "watch"} {
		if n[unwanted] {
			t.Errorf("host root should not have %q", unwanted)
		}
	}
	// "down" is a deprecated alias of "destroy", not its own top-level Name().
	if n["down"] {
		t.Error(`host root should not list "down" as a command name (it's an alias of "destroy")`)
	}
	destroy, _, err := NewRoot().Find([]string{"down"})
	if err != nil || destroy.Name() != "destroy" {
		t.Fatalf(`"down" must resolve to the "destroy" command via cobra Aliases; got %v, err %v`, destroy, err)
	}
}

// TestHostRootLeavesErrorPrintingToExecute: cobra's own `Error: <err>` line
// would relay guest-supplied text raw (scion stderr wrapped into an error,
// a hub slug), so the host root silences it; cli.Execute prints the
// sanitized line instead. Cobra's usage print on error stays as it was
// (every subcommand sets SilenceUsage itself).
func TestHostRootLeavesErrorPrintingToExecute(t *testing.T) {
	guest := "scion: \x1b]0;pwned\x07all good"
	newRoot := func() *cobra.Command {
		root := NewRoot()
		root.AddCommand(&cobra.Command{
			Use:          "boom",
			SilenceUsage: true,
			RunE:         func(*cobra.Command, []string) error { return errors.New(guest) },
		})
		return root
	}
	out, err := clitest.Exec(t, newRoot(), "boom")
	if err == nil || err.Error() != guest {
		t.Fatalf("Execute must still return the error unchanged: %v", err)
	}
	if out != "" {
		t.Fatalf("cobra must not print the error itself:\n%q", out)
	}

	root := newRoot()
	root.SetArgs([]string{"boom"})
	var stderr bytes.Buffer
	if code := cli.Execute(root, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got, want := stderr.String(), "Error: scion: all good\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}
