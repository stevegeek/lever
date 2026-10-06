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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/termsafe"
)

const Version = "0.29.1"

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
//
// Two builds get the binary's own content hash as well: one with no
// provenance at all, and a dirty one. Go stamps no VCS info for a build in a
// git worktree (its .git is a file, which Go's VCS detection does not
// accept), and every dirty build of one commit carries the same stamp. apply
// restarts the broker and the remote proxy when this string changes, so
// without the hash a new build of the same release kept the old daemons
// running.
func VersionString() string { return versionString() }

var versionString = sync.OnceValue(func() string {
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
	bin := ""
	if needsBinaryHash(rev, dirty, modVersion) {
		bin = executableHash()
	}
	return formatVersion(Version, rev, dirty, modVersion, bin)
})

// needsBinaryHash reports a build whose stamp does not tell builds apart: no
// commit and no module version, or a commit with uncommitted changes.
func needsBinaryHash(rev string, dirty bool, modVersion string) bool {
	if rev != "" {
		return dirty
	}
	return modVersion == "" || modVersion == "(devel)"
}

// executableHash is the first 12 hex digits of the running binary's SHA-256,
// or "" when the binary cannot be read.
func executableHash() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// formatVersion renders the version line from the release string plus whichever
// build provenance is available: a VCS commit (local builds) takes precedence
// over a module version (go install builds); with neither, just the release.
// bin, the binary's content hash, is appended when set (see VersionString).
func formatVersion(base, rev string, dirty bool, modVersion, bin string) string {
	var parts []string
	switch {
	case rev != "":
		short := rev
		if len(short) > 12 {
			short = short[:12]
		}
		if dirty {
			short += "-dirty"
		}
		parts = append(parts, short)
	case modVersion != "" && modVersion != "(devel)":
		parts = append(parts, modVersion)
	}
	if bin != "" {
		parts = append(parts, "bin "+bin)
	}
	if len(parts) == 0 {
		return base
	}
	return base + " (" + strings.Join(parts, ", ") + ")"
}

// Exit codes a command can ask for with WithExitCode. A script that must tell
// these cases apart reads the code, never the error text: the text can carry
// words the jail chose, an exit code cannot.
const (
	// ExitManagerResumeFailed: `lever up` / `lever apply` could not resume
	// the manager. The record and its conversation are kept; `lever up
	// --fresh` is the way to discard them.
	ExitManagerResumeFailed = 3
	// ExitHubRefusedResume: the hub refused the resume call (a token without
	// the lifecycle scope, or a phase that cannot be resumed now). The
	// manager is kept; do NOT answer this with --fresh.
	ExitHubRefusedResume = 4
)

// ExitError is an error that names the process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// WithExitCode wraps err so Execute exits with code.
func WithExitCode(err error, code int) error { return &ExitError{Code: code, Err: err} }

// Execute runs root and returns the process exit code: 0, the code of an
// ExitError, or 1 when a command returned any other error. The error line is printed here, not by cobra:
// an error can wrap text the jail chose (scion's stderr, a hub slug, a
// container status), and cobra's own `Error: <err>` would relay it raw. The
// line is passed through termsafe.Sanitize first. Execute sets
// SilenceErrors on root so cobra prints nothing; SilenceUsage is left to
// each command as before.
func Execute(root *cobra.Command, stderr io.Writer) int {
	root.SilenceErrors = true
	if err := root.Execute(); err != nil {
		fmt.Fprintln(stderr, "Error:", termsafe.Sanitize(err.Error()))
		var ee *ExitError
		if errors.As(err, &ee) && ee.Code > 0 {
			return ee.Code
		}
		return 1
	}
	return 0
}
