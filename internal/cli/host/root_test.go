package host

import (
	"bytes"
	"errors"
	"strings"
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
// sanitized line instead.
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

// TestRootVersionFlag: `lever --version` prints what `lever version` prints.
func TestRootVersionFlag(t *testing.T) {
	flag, err := clitest.Exec(t, newRootWith(defaultFactory), "--version")
	if err != nil {
		t.Fatalf("lever --version: %v", err)
	}
	verb, err := clitest.Exec(t, newRootWith(defaultFactory), "version")
	if err != nil {
		t.Fatalf("lever version: %v", err)
	}
	if strings.TrimSpace(flag) == "" || strings.TrimSpace(flag) != strings.TrimSpace(verb) {
		t.Fatalf("--version printed %q, version printed %q", flag, verb)
	}
}

// TestUsageOnlyForUsageErrors (lever#202): a command that fails at run time
// prints its error and no usage text, which read as a flag problem; cobra's
// own flag and argument errors, and a RunE's usageError, keep it.
func TestUsageOnlyForUsageErrors(t *testing.T) {
	const usage = "Usage:\n  lever up [CONFIG] [flags]"
	missing := t.TempDir() + "/nope/lever.yaml"
	for _, c := range []struct {
		name      string
		argv      []string
		wantUsage bool
	}{
		{"runtime failure", []string{"up", missing}, false},
		{"unknown flag", []string{"up", "--bogus"}, true},
		{"too many arguments", []string{"up", "a", "b"}, true},
		{"flag combination checked in RunE", []string{"up", "--no-backup"}, true},
		{"bad --action JSON", []string{"directive", "send", "hello", "--action", "{"}, true},
		{"invalid action", []string{"directive", "send", "hello", "--action", `{"kind":"nope"}`}, true},
		{"bad --not-before", []string{"directive", "send", "hello", "--instruction", "x", "--not-before", "soon"}, true},
		{"--not-before too far ahead", []string{"directive", "send", "hello", "--instruction", "x", "--not-before", "2999-01-01T00:00:00Z"}, true},
	} {
		out, err := clitest.Exec(t, NewRoot(), c.argv...)
		if err == nil {
			t.Fatalf("%s: no error", c.name)
		}
		usage := usage
		if c.argv[0] == "directive" {
			usage = "Usage:\n  lever directive send"
		}
		if got := strings.Contains(out, usage); got != c.wantUsage {
			t.Errorf("%s: usage printed = %v, want %v:\n%s", c.name, got, c.wantUsage, out)
		}
	}
}

// TestSilenceUsageOnRunErrors: every RunE in the tree is wrapped, nested
// ones too; success and a command that already silences usage are left as
// they are.
func TestSilenceUsageOnRunErrors(t *testing.T) {
	var fail, wrong error
	root := &cobra.Command{Use: "lever"}
	group := &cobra.Command{Use: "group"}
	leaf := &cobra.Command{Use: "leaf", RunE: func(*cobra.Command, []string) error { return fail }}
	flags := &cobra.Command{Use: "flags", RunE: func(*cobra.Command, []string) error { return wrong }}
	group.AddCommand(leaf, flags)
	root.AddCommand(group)
	silenceUsageOnRunErrors(root)

	fail = errors.New("boom")
	out, err := clitest.Exec(t, root, "group", "leaf")
	if err != fail || strings.Contains(out, "Usage:") || !leaf.SilenceUsage {
		t.Fatalf("runtime error: err=%v usage printed=%v", err, strings.Contains(out, "Usage:"))
	}
	wrong = usageError{errors.New("--a goes with --b")}
	out, err = clitest.Exec(t, root, "group", "flags")
	if err == nil || err.Error() != "--a goes with --b" || !strings.Contains(out, "Usage:") {
		t.Fatalf("usage error: err=%v out=%q", err, out)
	}
	// A command that silences usage itself keeps it silenced.
	leaf.SilenceUsage = true
	fail = nil
	if _, err := clitest.Exec(t, root, "group", "leaf"); err != nil || !leaf.SilenceUsage {
		t.Fatalf("a silenced command changed: err=%v silenced=%v", err, leaf.SilenceUsage)
	}
	leaf.SilenceUsage = false
	if out, err := clitest.Exec(t, root, "group", "leaf"); err != nil || out != "" || leaf.SilenceUsage {
		t.Fatalf("success: err=%v out=%q silenced=%v", err, out, leaf.SilenceUsage)
	}
}
