// Package apply turns an application config into an ordered bring-up plan and
// executes it. The plan is pure so the bring-up contract is testable without a
// live stack.
package apply

import "github.com/stevegeek/lever/internal/config"

// StepKind names a bring-up operation. The constants below are the complete
// set; run.step has a case for each and errors on any other value. Values are
// the step names printed by `lever apply --dry-run`.
type StepKind string

const (
	KindBrokerUp       StepKind = "broker-up"
	KindLoadImage      StepKind = "load-image"
	KindInitMachine    StepKind = "init-machine"
	KindConfigRegistry StepKind = "config-registry"
	KindScionServer    StepKind = "scion-server"
	// KindHubIdentity converges lever's hub identities on the live hub: the
	// admin, the controller, the project, the controller PAT and the remote
	// logins (see Deps.EnsureHubIdentity).
	KindHubIdentity          StepKind = "hub-identity"
	KindCredential           StepKind = "credential"
	KindRegisterProject      StepKind = "register-project"
	KindAgentTemplate        StepKind = "agent-template"
	KindMintManagerBootstrap StepKind = "mint-manager-bootstrap"
	KindStartManager         StepKind = "start-manager"
	// KindRemoteProxy daemonizes `lever remote serve` (see Deps.StartRemoteProxy).
	// Plan emits it only when remote is enabled, last: it signs in only logins
	// hub-identity has already invited and bound. Stopping a stale proxy when
	// remote is disabled is Run's job, not this step's.
	KindRemoteProxy StepKind = "remote-proxy"
	// KindWorkerAutostart resumes or starts one worker that sets autostart,
	// through the broker's operator socket (see Deps.AutostartWorker). Plan
	// emits one per such worker, after start-manager. A failure warns and
	// never fails the run.
	KindWorkerAutostart StepKind = "worker-autostart"
)

// Step is one named bring-up operation. TarPath is set on load-image steps
// only: the host archive the image ships in, or "" when the image comes from
// the host docker store.
type Step struct {
	Kind    StepKind
	Target  string
	TarPath string
}

type PlanOpts struct {
	// BrokerOnly reduces the plan to broker-up and mint-manager-bootstrap, for
	// the VM-level acceptance gate that drives lever-agent directly in the VM
	// without scion (init-machine would fail there: no scion binary).
	BrokerOnly bool
	// Fresh carries `lever up --fresh` into the start-manager step: a present
	// manager record, whatever its phase, is deleted and the manager created
	// anew. It lives here because after `lever stop` the hub is down until the
	// scion-server step, so `up` cannot delete the record beforehand.
	Fresh bool
	// Attach says the caller attaches the operator to the manager once Run
	// returns: start-manager's gate for a manager it created then ends as soon
	// as claude's tmux pane is alive, and the caller checks the manager after
	// the detach (CheckManagerAfterDetach). A resume's gate still waits for
	// claude's session start, so a --continue that finds no conversation is
	// caught before the attach.
	Attach bool
	// FreshWithoutConversation lets start-manager replace a resume that died
	// because claude --continue found no conversation with a fresh start, once
	// Deps.ManagerConversationEmpty proved from the guest that the home holds
	// no transcript. `lever apply` never sets it, so it never deletes a record.
	FreshWithoutConversation bool
}

var brokerOnlyKinds = map[StepKind]bool{
	KindBrokerUp:             true,
	KindMintManagerBootstrap: true,
}

// Plan returns the ordered bring-up for an app. Order is load-bearing: images
// must be loaded and projects registered before the manager starts. Run
// overlaps broker-up and load-image with later steps (see waitsFor). The jail
// itself is not a step: it is up before Run can be called.
func Plan(a *config.App, opts PlanOpts) []Step {
	steps := []Step{{Kind: KindBrokerUp}}
	// Worker images must be loaded now: they cannot be pulled under the egress
	// allowlist. Validate has already refused one ref from two different
	// archives. A ref named both with and without an archive loads from the
	// archive whatever the declaration order.
	seen := map[string]int{}
	addLoad := func(img, tar string) {
		if img == "" {
			return
		}
		if i, ok := seen[img]; ok {
			if steps[i].TarPath == "" {
				steps[i].TarPath = tar
			}
			return
		}
		seen[img] = len(steps)
		steps = append(steps, Step{Kind: KindLoadImage, Target: img, TarPath: tar})
	}
	addLoad(a.ManagerImage(), a.ManagerImageTarPath())
	for _, g := range a.Workers {
		addLoad(a.WorkerImage(g), a.WorkerImageTarPath(g))
	}
	steps = append(steps,
		Step{Kind: KindInitMachine},
		Step{Kind: KindConfigRegistry},
		Step{Kind: KindScionServer},
		// Needs the live hub, and mints the PAT every later hub call uses.
		Step{Kind: KindHubIdentity, Target: a.Tree},
	)
	if a.Manager.CredentialFile != "" {
		steps = append(steps, Step{Kind: KindCredential, Target: a.Manager.CredentialFile})
	}
	steps = append(steps, Step{Kind: KindRegisterProject, Target: a.Tree})
	// After register-project because the template's settings are written at
	// project scope; before start-manager because provisioning reads the
	// template (see Deps.EnsureAgentTemplate).
	steps = append(steps, Step{Kind: KindAgentTemplate, Target: a.Tree})
	// The enrol ticket is minted just before spawn so its TTL cannot run out.
	steps = append(steps, Step{Kind: KindMintManagerBootstrap, Target: a.Tree})
	steps = append(steps, Step{Kind: KindStartManager, Target: a.Name})
	for _, g := range a.Workers {
		if g.Autostart {
			steps = append(steps, Step{Kind: KindWorkerAutostart, Target: g.Name})
		}
	}
	if a.RemoteEnabled() {
		steps = append(steps, Step{Kind: KindRemoteProxy})
	}
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
