// Package cli holds what the two lever binaries share: the release Version
// constant and the `version` command. Code for a specific trust domain lives in
// cli/host (the operator's machine) or cli/manager (the agent container), so
// neither binary links the other's code.
//
// The release workflow greps `const Version = "..."` from this file; keep the
// constant here.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/buildstamp"
	"github.com/stevegeek/lever/internal/termsafe"
)

const Version = "0.51.0"

// VersionCmd returns the host `version` command.
func VersionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(VersionString()) }}
}

// VersionString is the release Version plus the build's provenance
// (buildstamp.Describe). apply restarts the broker and the remote proxy when it
// changes.
func VersionString() string { return buildstamp.Describe(Version) }

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
// ExitError, or 1 for any other error. Execute prints the error itself, through
// termsafe.Sanitize, and silences cobra's: an error can wrap text the jail chose
// (scion's stderr, a hub slug, a container status).
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
