// Package apply turns an application config into an ordered bring-up plan and
// (Part C) executes it. The plan is pure so the bring-up contract is testable
// without a live stack.
package apply

import "github.com/lever-to/lever/internal/config"

// Step is one named bring-up operation. Kind drives the executor; Target/Detail
// carry operands (a dir to register, the manager image, etc.).
type Step struct {
	Kind   string // jail-up | load-image | init-machine | config-registry | scion-server | credential | register-manager | register-grove | write-manifest | start-manager
	Target string
	Detail string
}

// Plan returns the ordered bring-up for an app. Order is load-bearing: the jail
// must exist and the image loaded before scion runs in it; projects must be
// registered before the manager (which orchestrates them) starts.
func Plan(a *config.App) []Step {
	steps := []Step{{Kind: "jail-up", Target: a.Tree}}
	// Load every distinct image into the jail's container runtime: the manager
	// image plus any grove that overrides it (groves default to the manager
	// image, which is then loaded once). Groves are started later by the
	// manager, so their images must already be present — they can't be pulled
	// under the egress allowlist. Dedup preserves first-seen order.
	seen := map[string]bool{}
	addLoad := func(img string) {
		if img != "" && !seen[img] {
			seen[img] = true
			steps = append(steps, Step{Kind: "load-image", Target: img})
		}
	}
	addLoad(a.Manager.Image)
	for _, g := range a.Groves {
		addLoad(a.GroveImage(g))
	}
	steps = append(steps,
		Step{Kind: "init-machine"},
		Step{Kind: "config-registry", Detail: "scionlocal"},
		Step{Kind: "scion-server"},
	)
	if a.Manager.CredentialFile != "" {
		steps = append(steps, Step{Kind: "credential", Target: a.Manager.CredentialFile})
	}
	steps = append(steps, Step{Kind: "register-manager", Target: a.Tree})
	for _, g := range a.Groves {
		steps = append(steps, Step{Kind: "register-grove", Target: a.GroveDir(g)})
	}
	// Write the sanitized runtime manifest (grove→image) into the mount so the
	// in-jail manager can resolve grove images without reading the operator
	// config (which stays host-only, outside the mount).
	steps = append(steps, Step{Kind: "write-manifest", Target: a.Tree})
	steps = append(steps, Step{Kind: "start-manager", Target: a.Name, Detail: a.Manager.Image})
	return steps
}
