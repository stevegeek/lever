// Package apply turns an application config into an ordered bring-up plan and
// (Part C) executes it. The plan is pure so the bring-up contract is testable
// without a live stack.
package apply

import "github.com/stevegeek/lever/internal/config"

// Step is one named bring-up operation. Kind drives the executor; Target/Detail
// carry operands (a dir to register, the manager image, etc.).
type Step struct {
	Kind   string // jail-up | broker-up | load-image | init-machine | config-registry | scion-server | credential | register-manager | register-grove | mint-manager-bootstrap | start-manager
	Target string
	Detail string
}

// PlanOpts controls optional Plan behaviour.
type PlanOpts struct {
	// BrokerOnly reduces the plan to the steps the VM-level acceptance gate
	// needs — jail-up (machine + egress allowlist), broker-up (host broker +
	// tools), and mint-manager-bootstrap (the manager enrol ticket) — and omits
	// ALL scion/container/registration steps (load-image, init-machine,
	// config-registry, scion-server, credential, register-*,
	// start-manager). The gate drives lever-agent directly in the VM, so scion is
	// never invoked; running init-machine on a fresh machine would fail (no scion
	// binary). The full container path is a later milestone.
	BrokerOnly bool
}

// brokerOnlyKinds is the allowlist of steps retained in BrokerOnly mode.
var brokerOnlyKinds = map[string]bool{
	"jail-up":                true,
	"broker-up":              true,
	"mint-manager-bootstrap": true,
}

// Plan returns the ordered bring-up for an app. Order is load-bearing: the jail
// must exist and the image loaded before scion runs in it; projects must be
// registered before the manager (which orchestrates them) starts.
func Plan(a *config.App, opts PlanOpts) []Step {
	steps := []Step{{Kind: "jail-up", Target: a.Tree}}
	// Bring the host broker (+ first-party tools) up early; the jail reaches it
	// at host.orb.internal. Health-checked before the manager starts.
	steps = append(steps, Step{Kind: "broker-up"})
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
	for _, g := range a.Workers {
		addLoad(a.WorkerImage(g))
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
	for _, g := range a.Workers {
		steps = append(steps, Step{Kind: "register-worker", Target: a.WorkerDir(g)})
	}
	// Mint the manager's one-time enrol ticket just before spawn (fresh, no TTL race).
	steps = append(steps, Step{Kind: "mint-manager-bootstrap", Target: a.Tree})
	steps = append(steps, Step{Kind: "start-manager", Target: a.Name, Detail: a.Manager.Image})
	if opts.BrokerOnly {
		filtered := steps[:0:0]
		for _, s := range steps {
			if brokerOnlyKinds[s.Kind] {
				filtered = append(filtered, s)
			}
		}
		return filtered
	}
	return steps
}
