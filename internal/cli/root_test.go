package cli

import (
	"bytes"
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
		name, base, rev string
		dirty           bool
		modVer, want    string
	}{
		{"commit", "0.5.0", "a8abdaef12345678", false, "", "0.5.0 (a8abdaef1234)"},
		{"commit-dirty", "0.5.0", "a8abdaef12345678", true, "", "0.5.0 (a8abdaef1234-dirty)"},
		{"short-commit-not-truncated", "0.5.0", "abc123", false, "", "0.5.0 (abc123)"},
		{"module-version-when-no-vcs", "0.5.0", "", false, "v0.5.0", "0.5.0 (v0.5.0)"},
		{"devel-module-ignored", "0.5.0", "", false, "(devel)", "0.5.0"},
		{"nothing-available", "0.5.0", "", false, "", "0.5.0"},
		{"commit-wins-over-module", "0.5.0", "deadbeef", true, "v0.5.0", "0.5.0 (deadbeef-dirty)"},
	} {
		if got := formatVersion(c.base, c.rev, c.dirty, c.modVer); got != c.want {
			t.Errorf("%s: formatVersion(%q,%q,%v,%q) = %q, want %q", c.name, c.base, c.rev, c.dirty, c.modVer, got, c.want)
		}
	}
}
