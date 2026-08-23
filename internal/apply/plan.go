// Package apply turns an application config into an ordered bring-up plan and
// (Part C) executes it. The plan is pure so the bring-up contract is testable
// without a live stack.
package apply

import "github.com/stevegeek/lever/internal/config"

// StepKind names a bring-up operation. It is a lever-owned enum: the constants
// below are the complete, authoritative set (runStep's dispatch has a case for
// each, plus a hard-error default). The type is a string alias, so untyped
// literals still convert implicitly — the protection is named-constants-at-every-
// site + find-refs, not compile-time exhaustiveness (the repo runs no exhaustive
// linter). Values are the on-the-wire step names printed by `lever apply --dry-run`.
type StepKind string

const (
	KindJailUp               StepKind = "jail-up"
	KindBrokerUp             StepKind = "broker-up"
	KindLoadImage            StepKind = "load-image"
	KindInitMachine          StepKind = "init-machine"
	KindConfigRegistry       StepKind = "config-registry"
	KindBootstrapToken       StepKind = "bootstrap-token"
	KindScionServer          StepKind = "scion-server"
	KindCredential           StepKind = "credential"
	KindRegisterProject      StepKind = "register-project"
	KindAgentTemplate        StepKind = "agent-template"
	KindMintManagerBootstrap StepKind = "mint-manager-bootstrap"
	KindStartManager         StepKind = "start-manager"
	// KindRemoteProxy daemonizes `lever remote serve` (see Deps.StartRemoteProxy).
	// Plan emits it ONLY when a.RemoteEnabled() — see Plan's remote-proxy block
	// — so dry-run output never advertises a start step that won't run. The
	// converse (remote disabled, a stale proxy left running by a prior apply)
	// is NOT this step's job: Run reconciles that itself, unconditionally,
	// after the step loop — see Run's doc in run.go.
	KindRemoteProxy StepKind = "remote-proxy"
)

// Step is one named bring-up operation. Kind drives the executor; Target
// carries the operand (a dir to register, the manager slug, etc.).
type Step struct {
	Kind   StepKind
	Target string
}

// PlanOpts controls optional Plan behaviour.
type PlanOpts struct {
	// BrokerOnly reduces the plan to the steps the VM-level acceptance gate
	// needs — jail-up (machine + egress allowlist), broker-up (host broker +
	// tools), and mint-manager-bootstrap (the manager enrol ticket) — and omits
	// ALL scion/container/registration steps (load-image, init-machine,
	// config-registry, bootstrap-token, scion-server, credential, register-*,
	// start-manager). The gate drives lever-agent directly in the VM, so scion is
	// never invoked; running init-machine on a fresh machine would fail (no scion
	// binary). The full container path is a later milestone.
	BrokerOnly bool
}

// brokerOnlyKinds is the allowlist of steps retained in BrokerOnly mode.
var brokerOnlyKinds = map[StepKind]bool{
	KindJailUp:               true,
	KindBrokerUp:             true,
	KindMintManagerBootstrap: true,
}

// Plan returns the ordered bring-up for an app. Order is load-bearing: the jail
// must exist and the image loaded before scion runs in it; projects must be
// registered before the manager (which orchestrates them) starts.
func Plan(a *config.App, opts PlanOpts) []Step {
	steps := []Step{{Kind: KindJailUp, Target: a.Tree}}
	// Bring the host broker (+ first-party tools) up early; the jail reaches it
	// at host.orb.internal. Health-checked before the manager starts.
	steps = append(steps, Step{Kind: KindBrokerUp})
	// Load every distinct image into the jail's container runtime: the manager
	// image plus any worker that overrides it (workers default to the manager
	// image, which is then loaded once). Workers are started later by the
	// manager, so their images must already be present — they can't be pulled
	// under the egress allowlist. Dedup preserves first-seen order.
	seen := map[string]bool{}
	addLoad := func(img string) {
		if img != "" && !seen[img] {
			seen[img] = true
			steps = append(steps, Step{Kind: KindLoadImage, Target: img})
		}
	}
	addLoad(a.ManagerImage())
	for _, g := range a.Workers {
		addLoad(a.WorkerImage(g))
	}
	steps = append(steps,
		Step{Kind: KindInitMachine},
		Step{Kind: KindConfigRegistry},
		// Mint (or reuse) the controller PAT the executor injects into the real,
		// dev-auth-off hub via SCION_HUB_TOKEN, BEFORE scion-server locks the hub
		// down. See Deps.EnsureControllerPAT.
		Step{Kind: KindBootstrapToken, Target: a.Tree},
		Step{Kind: KindScionServer},
	)
	if a.RemoteEnabled() {
		// The proxy reverse-proxies the hub's web API, so the hub must be
		// confirmed up first — scion-server, just above (see
		// Deps.StartRemoteProxy). Nothing later in the plan (credential,
		// register-project, start-manager) is a dependency of the proxy, so
		// this is the earliest correct placement.
		steps = append(steps, Step{Kind: KindRemoteProxy})
	}
	if a.Manager.CredentialFile != "" {
		steps = append(steps, Step{Kind: KindCredential, Target: a.Manager.CredentialFile})
	}
	steps = append(steps, Step{Kind: KindRegisterProject, Target: a.Tree})
	// Suppress scion's placeholder system prompt before anything can be
	// provisioned from it (see Deps.EnsureAgentTemplate). AFTER register-project
	// because the settings half writes at PROJECT scope, which needs the project
	// to exist; BEFORE start-manager because provisioning reads the template.
	steps = append(steps, Step{Kind: KindAgentTemplate, Target: a.Tree})
	// Mint the manager's one-time enrol ticket just before spawn (fresh, no TTL race).
	steps = append(steps, Step{Kind: KindMintManagerBootstrap, Target: a.Tree})
	steps = append(steps, Step{Kind: KindStartManager, Target: a.Name})
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
