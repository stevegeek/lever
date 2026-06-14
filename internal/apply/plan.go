// Package apply turns an application config into an ordered bring-up plan and
// (Part C) executes it. The plan is pure so the bring-up contract is testable
// without a live stack.
package apply

import "github.com/lever-to/lever/internal/config"

// Step is one named bring-up operation. Kind drives the executor; Target/Detail
// carry operands (a dir to register, the manager image, etc.).
type Step struct {
	Kind   string // jail-up | scion-server | hub-enable | credential | register-manager | register-grove | start-manager
	Target string
	Detail string
}

// Plan returns the ordered bring-up for an app. Order is load-bearing: the jail
// must exist before scion runs in it; projects must be registered before the
// manager (which orchestrates them) starts.
func Plan(a *config.App) []Step {
	steps := []Step{
		{Kind: "jail-up", Target: a.Tree},
		{Kind: "scion-server"},
		{Kind: "hub-enable"},
	}
	if a.Manager.CredentialFile != "" {
		steps = append(steps, Step{Kind: "credential", Target: a.Manager.CredentialFile})
	}
	steps = append(steps, Step{Kind: "register-manager", Target: a.Tree})
	for _, g := range a.Groves {
		steps = append(steps, Step{Kind: "register-grove", Target: a.GroveDir(g)})
	}
	steps = append(steps, Step{Kind: "start-manager", Target: a.Name, Detail: a.Manager.Image})
	return steps
}
