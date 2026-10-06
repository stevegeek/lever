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
	// The line starts with the release version and may carry a build-provenance
	// suffix (commit / -dirty / module version) depending on how the binary was
	// built — so assert the prefix, not exact equality.
	if got := out.String(); !strings.HasPrefix(got, Version) || !strings.HasSuffix(got, "\n") {
		t.Fatalf("version output %q should start with %q and end with a newline", got, Version)
	}
}

func TestFormatVersion(t *testing.T) {
	for _, c := range []struct {
		name, base, rev   string
		dirty             bool
		modVer, bin, want string
	}{
		{"commit", "0.5.0", "a8abdaef12345678", false, "", "", "0.5.0 (a8abdaef1234)"},
		{"commit-dirty", "0.5.0", "a8abdaef12345678", true, "", "", "0.5.0 (a8abdaef1234-dirty)"},
		{"short-commit-not-truncated", "0.5.0", "abc123", false, "", "", "0.5.0 (abc123)"},
		{"module-version-when-no-vcs", "0.5.0", "", false, "v0.5.0", "", "0.5.0 (v0.5.0)"},
		{"devel-module-ignored", "0.5.0", "", false, "(devel)", "", "0.5.0"},
		{"nothing-available", "0.5.0", "", false, "", "", "0.5.0"},
		{"commit-wins-over-module", "0.5.0", "deadbeef", true, "v0.5.0", "", "0.5.0 (deadbeef-dirty)"},
		{"worktree-build-gets-binary-hash", "0.5.0", "", false, "(devel)", "1a2b3c4d5e6f", "0.5.0 (bin 1a2b3c4d5e6f)"},
		{"dirty-build-gets-binary-hash", "0.5.0", "deadbeef", true, "", "1a2b3c4d5e6f", "0.5.0 (deadbeef-dirty, bin 1a2b3c4d5e6f)"},
	} {
		if got := formatVersion(c.base, c.rev, c.dirty, c.modVer, c.bin); got != c.want {
			t.Errorf("%s: formatVersion(%q,%q,%v,%q,%q) = %q, want %q", c.name, c.base, c.rev, c.dirty, c.modVer, c.bin, got, c.want)
		}
	}
}

// TestNeedsBinaryHash: only a build whose stamp cannot tell two builds apart
// pays for hashing the binary — a clean commit and a tagged module do not.
func TestNeedsBinaryHash(t *testing.T) {
	for _, c := range []struct {
		rev    string
		dirty  bool
		modVer string
		want   bool
	}{
		{"deadbeef", false, "", false},
		{"deadbeef", true, "", true},
		{"", false, "v0.5.0", false},
		{"", false, "(devel)", true},
		{"", false, "", true},
	} {
		if got := needsBinaryHash(c.rev, c.dirty, c.modVer); got != c.want {
			t.Errorf("needsBinaryHash(%q,%v,%q) = %v, want %v", c.rev, c.dirty, c.modVer, got, c.want)
		}
	}
	if h := executableHash(); len(h) != 12 {
		t.Fatalf("executableHash() = %q, want 12 hex digits of the test binary", h)
	}
}

// TestExecutePrintsErrorsSanitized: a returned error is printed by Execute,
// not cobra, so guest-supplied text in it (a scion stderr line, a hub slug)
// reaches the terminal with its escape sequences stripped. The exit code
// stays 1 on error, 0 otherwise, and success prints nothing.
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
