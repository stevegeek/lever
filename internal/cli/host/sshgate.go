package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

// sshOffPrefix leads every message that says why `lever ssh` is not
// available.
const sshOffPrefix = "lever ssh"

// errNoConduitControls is the refusal on a scion without the hub's conduit
// controls.
var errNoConduitControls = errors.New("lever ssh needs a scion with the hub's conduit controls " +
	"(server.hub.conduit.disable_ssh, deny_ports and disable_ssh_socket_forwarding); conduit stays off")

// conduitScion is what the gate asks the jail.
type conduitScion interface {
	ConduitControls(ctx context.Context) (bool, error)
	LiveHub(ctx context.Context) (conduit.LiveHub, error)
}

// jailConduit asks the jail's scion and its running hub.
type jailConduit struct{ jr proc.Runner }

// jailScionBin is the scion the jail runner finds on its PATH (lever
// installs it root-owned in /usr/local/bin).
const jailScionBin = "scion"

func (j jailConduit) ConduitControls(ctx context.Context) (bool, error) {
	return conduit.Controls(ctx, j.jr, jailScionBin)
}

func (j jailConduit) LiveHub(ctx context.Context) (conduit.LiveHub, error) {
	return conduit.ReadLiveHub(ctx, j.jr)
}

// sshGate decides whether conduit may be on for `lever ssh`, and holds the
// proofs the experiments pin repeats around turning it on. Conduit is on only
// when the operator opted in (ssh.enabled), the jail's scion has the conduit
// controls, every agent image's sciontool enforces the policy at its end, and
// the live hub runs with lever's policy (policyLive). Anything lever cannot
// show keeps it off.
type sshGate struct {
	enabled bool
	// controls: the jail's scion validates the policy settings. It decides
	// whether the hub starts with the policy env at all.
	controls bool
	// on: conduit is wanted on in this apply.
	on bool
	// why says, when enabled and not on, why not.
	why    string
	policy conduit.Policy
	sc     conduitScion
}

// newSSHGate probes the jail. ssh.enabled off asks the jail's scion only
// whether it has the controls (the policy env is set whenever it does).
func newSSHGate(ctx context.Context, app *config.App, sc conduitScion, jr proc.Runner) sshGate {
	g := sshGate{enabled: app.SSHEnabled(), policy: apply.ConduitPolicy(app), sc: sc}
	controls, err := sc.ConduitControls(ctx)
	g.controls = err == nil && controls
	if !g.enabled {
		return g
	}
	switch {
	case err != nil:
		g.why = fmt.Sprintf("%s: %v; conduit stays off", sshOffPrefix, err)
		return g
	case !controls:
		g.why = errNoConduitControls.Error()
		return g
	}
	if err := agentImagesEnforcePolicy(ctx, jr, app); err != nil {
		g.why = fmt.Sprintf("%s: %v; conduit stays off", sshOffPrefix, err)
		return g
	}
	g.on = true
	return g
}

// policyLive returns nil only when a hub runs with an environment that sets
// at least lever's policy (conduit.Policy.Covers).
func (g sshGate) policyLive(ctx context.Context) error {
	if g.sc == nil {
		return errors.New("no scion to ask")
	}
	live, err := g.sc.LiveHub(ctx)
	if err != nil {
		return fmt.Errorf("reading the running hub: %w", err)
	}
	if !live.Running {
		return errors.New("no hub is running")
	}
	if !g.policy.Covers(live.Env) {
		return errors.New("the running hub was not started with lever's conduit policy")
	}
	return nil
}

// agentImages are the distinct images lever's agents run, by the name the
// jail's podman holds them under.
func agentImages(app *config.App) []string {
	var out []string
	add := func(ref string) {
		if ref != "" && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	add(app.ManagerAgentImage())
	for _, w := range app.Workers {
		add(app.WorkerAgentImage(w))
	}
	return out
}

// sciontoolPolicyMarker is the variable a sciontool that enforces the hub's
// conduit policy reads; an older one holds no such string and ignores the
// variable, serving SSH forwards to every port.
const sciontoolPolicyMarker = conduit.AgentPolicyEnv

// imagePolicyScript exits 0 when the image's sciontool holds the policy
// marker, 3 when the jail has no such image, 4 when the image has no
// sciontool, and 5 when it holds no marker. It runs nothing of the image's
// but sh, command and grep, with no network.
var imagePolicyScript = `podman image exists "$1" || exit 3
exec podman run --rm --pull=never --network=none ` + jail.ScionKeepIDUserns + ` --entrypoint /bin/sh "$1" -c ` +
	`'p=$(command -v sciontool) || exit 4; grep -qaF ` + sciontoolPolicyMarker + ` "$p" || exit 5'`

// agentImagesEnforcePolicy checks every agent image in the jail. An image the
// jail does not hold yet (a first apply loads images alongside this check)
// counts as not shown, so conduit waits for the next apply.
func agentImagesEnforcePolicy(ctx context.Context, jr proc.Runner, app *config.App) error {
	for _, img := range agentImages(app) {
		res, err := jr.Run(ctx, nil, "sh", "-c", imagePolicyScript, "_", img)
		if err == nil && res.Code == 0 {
			continue
		}
		switch res.Code {
		case 3:
			return fmt.Errorf("the jail does not hold agent image %s yet, so lever cannot check its sciontool (run `lever apply` again once it is loaded)", img)
		case 4:
			return fmt.Errorf("agent image %s has no sciontool on its PATH", img)
		case 5:
			return fmt.Errorf("the sciontool in agent image %s does not enforce the hub's conduit policy (rebuild the image on a scion with the conduit controls)", img)
		}
		return fmt.Errorf("could not check the sciontool in agent image %s: %v: %s", img, err, strings.TrimSpace(res.Stderr))
	}
	return nil
}
