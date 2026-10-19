package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestVersionCommand(t *testing.T) {
	root := &cobra.Command{Use: "lever"}
	root.AddCommand(VersionCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The version may carry a build-provenance suffix, so assert the prefix.
	if got := out.String(); !strings.HasPrefix(got, Version) || !strings.HasSuffix(got, "\n") {
		t.Fatalf("version output %q should start with %q and end with a newline", got, Version)
	}
}

// TestExecutePrintsErrorsSanitized: guest-supplied text in an error reaches
// the terminal with its escape sequences stripped.
func TestExecutePrintsErrorsSanitized(t *testing.T) {
	newRoot := func(err error) *cobra.Command {
		root := &cobra.Command{Use: "lever"}
		root.AddCommand(&cobra.Command{
			Use:          "boom",
			SilenceUsage: true,
			RunE:         func(*cobra.Command, []string) error { return err },
		})
		root.SetArgs([]string{"boom"})
		return root
	}
	var stderr bytes.Buffer
	root := newRoot(errors.New("scion: \x1b]0;pwned\x07\x1b[2K\x1b[1;32mall good\x1b[0m"))
	root.SetOut(&stderr) // usage, were it printed, would land here too
	root.SetErr(&stderr)
	if code := Execute(root, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got, want := stderr.String(), "Error: scion: all good\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	stderr.Reset()
	if code := Execute(newRoot(nil), &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("success must print nothing: %q", stderr.String())
	}
}

// TestExecuteExitCode: an ExitError decides the process exit code; any other
// error is 1.
func TestExecuteExitCode(t *testing.T) {
	run := func(err error) int {
		root := &cobra.Command{Use: "lever", RunE: func(*cobra.Command, []string) error { return err }, SilenceUsage: true}
		root.SetArgs(nil)
		return Execute(root, io.Discard)
	}
	if got := run(errors.New("plain")); got != 1 {
		t.Fatalf("plain error: exit %d, want 1", got)
	}
	if got := run(fmt.Errorf("step x: %w", WithExitCode(errors.New("kept"), ExitManagerResumeFailed))); got != ExitManagerResumeFailed {
		t.Fatalf("wrapped ExitError: exit %d, want %d", got, ExitManagerResumeFailed)
	}
	if got := run(nil); got != 0 {
		t.Fatalf("no error: exit %d", got)
	}
}
