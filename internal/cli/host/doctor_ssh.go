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
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// sshDoctor is checkSSH's seams.
type sshDoctor struct {
	sc      conduitScion
	hc      *hubapi.Client
	list    agentLister
	env     envReader
	project string
	agents  []string // the manager, then every worker
}

// checkSSH is the `lever ssh` row, shown only while ssh.enabled is set: on a
// scion without the conduit controls it says why conduit stays off; on one
// with them it checks what apply proved (conduit on, the hub's policy) and
// which running agents were started with the policy, the only ones that serve
// SSH under it.
func checkSSH(ctx context.Context, app *config.App, st state.State, d sshDoctor) checkResult {
	const name = "lever ssh"
	if !app.SSHEnabled() {
		return checkResult{}
	}
	if err := sshAllowed(app, st); err != nil {
		return checkResult{name, false, err.Error(), "point `tree:` at a subdirectory that does not contain " + state.DirName + "/"}
	}
	controls, err := d.sc.ConduitControls(ctx)
	switch {
	case err != nil:
		return checkResult{name, false, firstLine(err.Error()) + "; conduit stays off", "is the machine up? `lever apply`"}
	case !controls:
		if served, err := d.hc.RouteServed(ctx, conduitProbeRoute()); err == nil && served {
			return checkResult{name, false, "the hub serves conduit, but the jail's scion has no conduit controls to restrict it",
				"run `lever apply`: it pins conduit off and restarts the hub"}
		}
		return warnResult(name, errNoConduitControls.Error(), "pin a scion that has them (scion.version), then `lever apply`; until then `lever ssh` refuses")
	}
	rec, _, err := st.LoadHubExperimentsRecord()
	if err != nil {
		return checkResult{name, false, err.Error(), "remove " + stateDirName() + "/hub-experiments.json, then run `lever apply`"}
	}
	if !slices.Contains(rec.On, conduitExperiment) {
		return warnResult(name, "ssh.enabled is set but lever has not turned conduit on: the last apply could not prove it safe (its warnings say why)",
			"run `lever apply`")
	}
	g := sshGate{policy: apply.ConduitPolicy(app), sc: d.sc}
	if err := g.policyLive(ctx); err != nil {
		return checkResult{name, false, "conduit is recorded on, but " + err.Error(), "run `lever apply`; until then `lever ssh` refuses"}
	}
	served, err := d.hc.RouteServed(ctx, conduitProbeRoute())
	if err != nil {
		return checkResult{name, true, "not checked (hub not reachable): " + firstLine(err.Error()), ""}
	}
	if !served {
		return checkResult{name, false, "conduit is recorded on, but the hub does not serve it", "run `lever apply`"}
	}
	want := g.policy.AgentValue()
	var with, without, unread []string
	if d.list != nil && d.env != nil {
		agents, err := d.list(ctx, d.project)
		if err == nil {
			for _, a := range d.agents {
				rec := scionpkg.FindAgent(agents, a)
				if rec == nil || rec.ContainerID == "" {
					continue
				}
				v, set, err := d.env(ctx, rec.ContainerID, conduit.AgentPolicyEnv)
				switch {
				case errors.Is(err, jail.ErrNoContainer):
				case err != nil:
					unread = append(unread, a)
				case set && v == want:
					with = append(with, a)
				default:
					without = append(without, a)
				}
			}
		}
	}
	detail := fmt.Sprintf("conduit on, the hub denies ports %s to every tunnel and forward, SSH socket forwarding off", joinPorts(g.policy.DenyPorts))
	if len(with) > 0 {
		detail += "; reachable: " + strings.Join(with, ", ")
	}
	if len(unread) > 0 {
		detail += "; not checked: " + strings.Join(unread, ", ")
	}
	if len(without) > 0 {
		return warnResult(name, detail+"; started before conduit was on or under another policy: "+strings.Join(without, ", "),
			"restart them (`lever stop` and `lever up` for the manager) so they serve SSH under this policy")
	}
	return checkResult{name, true, detail, ""}
}

func joinPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ",")
}

// conduitProbeRoute is the route the hub serves exactly while conduit is on.
func conduitProbeRoute() string {
	for _, e := range pinnedOffExperiments {
		if e.name == conduitExperiment {
			return e.probe
		}
	}
	return ""
}
