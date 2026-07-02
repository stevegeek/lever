package registry

import (
	"fmt"
	"strings"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/jail"
)

// JailRunner rebuilds the command transport into a jail from its already-
// resolved identity (machine, run user, uid) WITHOUT needing EnsureUp state.
// The broker uses this for host-side grove dispatch: `lever apply` resolved the
// identity and passed it via env; the broker process reconstructs the transport
// here. Task-adding a backend: extend the switch (the lockstep with
// constructors is exercised by TestJailRunnerCoversAllCandidates).
func JailRunner(name string, host exec.Runner, machine, user, uid string) (exec.Runner, error) {
	if name == "" {
		name = Default
	}
	switch name {
	case "orbstack":
		return jail.New(host, orbstack.JailPrefix(machine, user), uid), nil
	}
	return nil, fmt.Errorf("unknown backend %q (valid: %s)", name, strings.Join(backend.Names(), ", "))
}
