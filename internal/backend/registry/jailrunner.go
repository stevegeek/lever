package registry

import (
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/backend/orbstack"
	"github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/jail"
)

// JailArgv returns the argv prefix that runs a command INSIDE the jail for the
// named backend, from an already-resolved identity — no EnsureUp state, no
// I/O. The remote proxy's jail-dial transport needs the bare prefix rather
// than a JailRunner: it execs `<prefix> nc host port` and adapts the child's
// pipes to a net.Conn, which exec.Runner's run-to-completion, capture-the-
// output contract cannot express.
//
// This switch is one of the three backend lockstep points (with the
// constructors table and backend.Candidates — see the package doc); JailRunner
// is built on it. TestJailArgvCoversAllCandidates keeps it in step.
func JailArgv(name, machine, user string) ([]string, error) {
	if name == "" {
		name = Default
	}
	switch name {
	case "orbstack":
		return orbstack.JailPrefix(machine, user), nil
	case "lima":
		return lima.JailPrefix(machine), nil
	}
	return nil, fmt.Errorf("unknown backend %q (valid: %s)", name, strings.Join(backend.Names(), ", "))
}

// JailRunner rebuilds the command transport into a jail from its already-
// resolved identity (machine, run user, uid) WITHOUT needing EnsureUp state.
// The broker uses this for host-side worker dispatch: `lever apply` resolved the
// identity and passed it via env; the broker process reconstructs the transport
// here.
func JailRunner(name string, host exec.Runner, machine, user, uid string) (exec.Runner, error) {
	argv, err := JailArgv(name, machine, user)
	if err != nil {
		return nil, err
	}
	return jail.New(host, argv, uid), nil
}
