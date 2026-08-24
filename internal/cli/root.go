// Package cli holds what the two lever binaries share: the release Version
// constant and the `version` command. Everything that runs in a specific
// trust domain lives in a subpackage — cli/host for the operator's machine
// (`lever`) and cli/manager for the agent container (`lever-manager`) — so
// that neither binary links the other's code.
//
// The release workflow greps `const Version = "..."` from this file; keep the
// constant here.
package cli

import (
	"runtime/debug"

	"github.com/spf13/cobra"
)

const Version = "0.19.0"

// VersionCmd returns the `version` command both binaries register.
func VersionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(VersionString()) }}
}

// VersionString augments the hardcoded release Version with Go's embedded VCS
// stamp when present: the commit the binary was built from (short) plus a
// "-dirty" marker for an uncommitted tree, or — for a `go install module@vX`
// build, which carries no VCS stamp — the module version. This stops `lever
// version` from masking a stale or local build behind the bare release string
// (a make-install binary can lag the source it was built from, which the
// hardcoded const alone hides).
func VersionString() string {
	var rev, modVersion string
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		modVersion = info.Main.Version
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	return formatVersion(Version, rev, dirty, modVersion)
}

// formatVersion renders the version line from the release string plus whichever
// build provenance is available: a VCS commit (local builds) takes precedence
// over a module version (go install builds); with neither, just the release.
func formatVersion(base, rev string, dirty bool, modVersion string) string {
	switch {
	case rev != "":
		short := rev
		if len(short) > 12 {
			short = short[:12]
		}
		if dirty {
			short += "-dirty"
		}
		return base + " (" + short + ")"
	case modVersion != "" && modVersion != "(devel)":
		return base + " (" + modVersion + ")"
	default:
		return base
	}
}
