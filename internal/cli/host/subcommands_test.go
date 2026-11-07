package host

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every command that only groups subcommands refuses an unknown one with an
// error, never its help and exit 0 (lever broker revoke is not a command),
// and still prints its help when run bare.
func TestGroupCommandsRefuseUnknownSubcommands(t *testing.T) {
	var groups []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.HasParent() && c.HasSubCommands() {
			groups = append(groups, c)
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(NewRoot())
	if len(groups) == 0 {
		t.Fatal("no group commands found")
	}
	for _, g := range groups {
		path := strings.Fields(g.CommandPath())[1:]
		root := NewRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append(append([]string{}, path...), "no-such-verb"))
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), `unknown command "no-such-verb"`) {
			t.Errorf("%s no-such-verb: err = %v", g.CommandPath(), err)
		}
		root = NewRoot()
		out.Reset()
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(path)
		if err := root.Execute(); err != nil || !strings.Contains(out.String(), "Usage:") {
			t.Errorf("%s bare: err = %v, output %q", g.CommandPath(), err, out.String())
		}
	}
}
