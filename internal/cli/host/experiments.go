package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/state"
)

// pinnedExperiment is a scion hub experiment lever keeps off, with a read-only
// route the hub answers 404 exactly while the experiment is off. An older scion
// lacks the route and answers 404 too, which is also "off". The controller PAT
// and lever-admin can read every probe route; only lever-admin reads the admin
// view. An experiment with no probe is proved off by the admin view alone.
type pinnedExperiment struct {
	name  string
	probe string
}

// pinnedOffExperiments are the experiments lever pins off, so that a changed
// scion default cannot turn one on. hub.conduit adds an inbound shell, sftp
// and port path into every agent, reaching the jail loopback and any Unix
// socket; hub.artifacts adds hub-held publishing with unauthenticated share
// links beside lever's reviewed file paths. federation.project_visitors
// admits users of external issuers and mints this hub's users credentials for
// peer hubs. hub.attachment_artifacts copies chat attachments into agent
// inboxes and the project scratchpad, and scion turns it on by default
// wherever its artifact service is ready, local disk included.
// hub.port_view serves an agent's ports, WebSockets included, to anyone with
// the URL. hub.conduit_deliver delivers messages to agents over a conduit
// session instead of runtime exec. hub.agent_plugins installs plugin bundles a
// template declares into the harness (claude included) at every start.
// hub.agent_delegation lets a user grant agents hub calls under the user's
// name, through sciontool's delegation socket in every agent.
// hub.async_agent_start is pinned for correctness, not reach
// (asyncAgentStartExperiment).
//
// From scion d34ffbc1: hub.triggers adds webhook endpoints and triggers that
// agents can author, an unauthenticated signed ingress (/api/v1/hooks/{id})
// that messages agents, and actions that dispatch new agents outside the
// broker; hub.github_triggers feeds GitHub App deliveries into them.
// hub.exec_schedules runs commands in an agent's container from a schedule or
// trigger, with no LLM turn, whenever conduit is on. hub.exec_containers lets
// users start shell containers in the jail outside lever's agent model.
// hub.system_one makes model-provider calls from the hub process itself.
// hub.code_span_mentions turns an inline code span such as `@name` into a
// mention, which would route a contact's message to another agent.
// hub.agent_secrets lets an agent write its own secrets, delivered at start
// above every secret lever sets, and ask a user to type a secret into a form.
// lifecycle.idle_suspend lets the hub suspend idle agents on its own schedule
// and wake a hub-suspended agent for any sender, past the broker's wake
// checks. hub.suspend_screen (on by default) stores the agent's last terminal
// screen in the hub database at every hub-run suspend.
//
// From scion d6d6abc7: hub.command_requests lets an agent ask a user to
// approve a shell command that the hub then runs in a container over
// conduit, which lever turns on for `lever ssh`. hub.template_drift_warning
// makes outbound GitHub calls from the hub at agent creation.
// hub.agent_move_bundle carries an agent's workspace and home through hub
// storage to another broker. tokenomics.usage_ledger adds an agent-to-hub
// usage data path; tokenomics.token_budgets and tokenomics.trajectory_capture
// (budget admission and breach actions; transcript capture and harvest in
// agents) are pinned before scion builds them, so no later default turns them
// on. lifecycle.never_suspend lets an agent record refuse lever's suspend.
// hub.artifacts_transfer hands an artifact to another agent.
// hub.decision_retention lets an admin purge settled decisions (web.decisions
// is pinned off, so there are none); it is pinned with the rest so the set
// lever reviewed stays the set that can run.
//
// From scion ddb48b02: hub.mcp_server serves an MCP endpoint (send and read
// messages, event push) on a listener of its own; lever never configures that
// listener, and the pin is the second lock. hub.large_uploads takes resumable
// uploads of up to 1 GiB into a project's workspace and shared directories.
// agent_policy.telemetry_enforcement lets an admin force every agent's
// telemetry export on. agent_policy.duration_backstop stamps run deadlines
// that stop agents, with agent_policy.limit_ceilings; both only restrict, and
// they are pinned with hub.artifacts_review_requests and
// hub.flat_runtime_brokers (older, on by default from ddb48b02: single-target
// broker registrations and agent pinning to a target; lever's broker is not
// one) so the set lever reviewed stays the set that can run.
// web.agents_column_picker is display only and left alone. ddb48b02 also turns hub.conduit,
// hub.artifacts and hub.agent_plugins on by default, which these pins hold
// off.
//
// hub.chat_thread_groups (project-wide chat groups that agents can create and
// rename) is graduated from e8160a10 on: always on, never registered, and
// scion refuses an override for it. It stays here for older pins, where the
// admin view still registers it and lever pins it off; it has no probe,
// because its route is served on every newer hub. The remote proxy refuses
// its routes either way.
//
// Only conduit, artifacts and triggers have a probe: a read-only route the
// hub answers 404 exactly while the experiment is off (the triggers ingress
// takes no credential, and answers 405 to a GET while on). (federation gained
// GET routes in e8160a10, but an older pin has none, so it stays unprobed.)
// The others are proved off by the admin view alone (pinExperiments re-reads
// it after the write). scion reads all of them per request from the hub
// database, except conduit's relay, which starts only with the hub.
var pinnedOffExperiments = []pinnedExperiment{
	{name: "hub.conduit", probe: "/api/v1/conduit/grant-keys"},
	{name: "hub.artifacts", probe: "/api/v1/artifacts?mine=1"},
	{name: threadGroupsExperiment},
	{name: "federation.project_visitors"},
	{name: "hub.attachment_artifacts"},
	{name: "hub.port_view"},
	{name: "hub.conduit_deliver"},
	{name: "hub.agent_plugins"},
	{name: "hub.agent_delegation"},
	{name: asyncAgentStartExperiment},
	{name: "hub.triggers", probe: "/api/v1/hooks/00000000-0000-0000-0000-000000000000"},
	{name: "hub.github_triggers"},
	{name: "hub.exec_schedules"},
	{name: "hub.exec_containers"},
	{name: "hub.system_one"},
	{name: "hub.code_span_mentions"},
	{name: "hub.agent_secrets"},
	{name: "lifecycle.idle_suspend"},
	{name: "hub.suspend_screen"},
	{name: "hub.message_policies"},
	{name: "web.decisions"},
	{name: "hub.runtime_orphan_reconcile"},
	{name: "hub.reincarnate_harness_switch"},
	{name: "hub.custom_default_user_role"},
	{name: "hub.command_requests"},
	{name: "hub.template_drift_warning"},
	{name: "hub.agent_move_bundle"},
	{name: "tokenomics.usage_ledger"},
	{name: "tokenomics.token_budgets"},
	{name: "tokenomics.trajectory_capture"},
	{name: "lifecycle.never_suspend"},
	{name: "hub.artifacts_transfer"},
	{name: "hub.decision_retention"},
	{name: "hub.mcp_server"},
	{name: "hub.large_uploads"},
	{name: "agent_policy.telemetry_enforcement"},
	{name: "agent_policy.limit_ceilings"},
	{name: "agent_policy.duration_backstop"},
	{name: "hub.artifacts_review_requests"},
	{name: "hub.flat_runtime_brokers"},
}

// accessConstraintsExperiment (scion d6d6abc7 on, default off) serves the
// access-constraint admin API, which lever caps every remote user with
// (ensureRemoteCeiling); while it is off those routes answer 404 and apply
// stops at the cap. Stored constraints are enforced whether or not it is on,
// so on widens nothing: it serves a hub-admin route.
const accessConstraintsExperiment = "hub.access_constraints"

// pinnedOnExperiments are the experiments lever pins on wherever the hub
// registers them, against a changed default or an admin's override. An older
// scion that does not register one serves its routes ungated.
var pinnedOnExperiments = []string{accessConstraintsExperiment}

// pinnedNames are every experiment lever pins, off and on.
func pinnedNames() []string {
	return append(pinnedOffNames(), pinnedOnExperiments...)
}

// asyncAgentStartExperiment (scion e8160a10 on, default on) starts, resumes
// and restarts existing agents asynchronously. That path does not store a
// resume's task in the record, so after `lever conversation new` the hub
// kept the old boot prompt, which a later resume of a stopped manager would
// start with. Off, resumes take the synchronous path, which stores it.
const asyncAgentStartExperiment = "hub.async_agent_start"

// threadGroupsExperiment is graduated (always on) from scion e8160a10 on.
const threadGroupsExperiment = "hub.chat_thread_groups"

// experimentsScope is the UAT scope of scion's experiments admin route. The
// jail's scion lists it exactly when it has hub-wide experiments, and answers
// with the hub down.
const experimentsScope = "hub_experiments:update"

var errExperimentsMalformed = errors.New("the hub's stored experiment settings are unreadable")

func pinnedOffNames() []string {
	out := make([]string, len(pinnedOffExperiments))
	for i, e := range pinnedOffExperiments {
		out[i] = e.name
	}
	return out
}

// hubHasExperiments reports whether the jail's scion has hub-wide
// experiments. When scion cannot be asked, a record of an earlier pin decides.
func hubHasExperiments(ctx context.Context, known scopeKnownFunc, rec state.HubExperimentsRecord, found bool) bool {
	has, _ := hubExperimentsSupport(ctx, known, rec, found)
	return has
}

// hubExperimentsSupport is hubHasExperiments with sure false when neither the
// jail's scion nor a record answered.
func hubExperimentsSupport(ctx context.Context, known scopeKnownFunc, rec state.HubExperimentsRecord, found bool) (has, sure bool) {
	if known != nil {
		if has, err := known(ctx, experimentsScope); err == nil {
			return has, true
		}
	}
	if found {
		return !rec.NotApplicable, true
	}
	return false, false
}

// liveExperiments returns the pinned-off experiments the hub serves now. Any
// answer but 404 counts as served: scion gates each probe route on its
// experiment before anything else can refuse the request.
func liveExperiments(ctx context.Context, hc *hubapi.Client) ([]string, error) {
	var live []string
	for _, e := range pinnedOffExperiments {
		if e.probe == "" {
			continue
		}
		served, err := hc.RouteServed(ctx, e.probe)
		if err != nil {
			return nil, fmt.Errorf("probing %s: %w", e.name, err)
		}
		if served {
			live = append(live, e.name)
		}
	}
	return live, nil
}

// pinConflictRetries bounds re-reads after a concurrent write to the
// experiments row.
const pinConflictRetries = 3

// conduitExperiment is the one pinned experiment lever turns on, and only
// for `lever ssh` (sshGate).
const conduitExperiment = "hub.conduit"

// pinExperimentsOff is pinExperiments with conduit off.
func pinExperimentsOff(ctx context.Context, hc *hubapi.Client, now time.Time) (state.HubExperimentsRecord, error) {
	return pinExperiments(ctx, hc, now, false)
}

// pinExperiments writes an explicit override for each pinned experiment the
// hub registers and does not already hold at the wanted value (false; true for
// pinnedOnExperiments, and for conduit when conduitOn), leaving every other override as it is, then
// requires the hub's answer to show each one overridden that way.
func pinExperiments(ctx context.Context, hc *hubapi.Client, now time.Time, conduitOn bool) (state.HubExperimentsRecord, error) {
	wantOn := func(name string) bool {
		return slices.Contains(pinnedOnExperiments, name) || conduitOn && name == conduitExperiment
	}
	for attempt := 1; ; attempt++ {
		cur, err := hc.AdminExperiments(ctx)
		if errors.Is(err, hubapi.ErrNoExperiments) {
			if conduitOn {
				return state.HubExperimentsRecord{}, fmt.Errorf("the hub has no experiments, so lever cannot turn %s on", conduitExperiment)
			}
			return state.HubExperimentsRecord{NotApplicable: true, PinnedAt: now}, nil
		}
		if err != nil {
			return state.HubExperimentsRecord{}, err
		}
		if cur.Malformed {
			return state.HubExperimentsRecord{}, errExperimentsMalformed
		}
		var registered []string
		want := map[string]bool{}
		for _, name := range pinnedNames() {
			e, ok := cur.Lookup(name)
			if !ok {
				continue
			}
			registered = append(registered, name)
			if e.Override == nil || *e.Override != wantOn(name) {
				want[name] = wantOn(name)
			}
		}
		if conduitOn && !slices.Contains(registered, conduitExperiment) {
			return state.HubExperimentsRecord{}, fmt.Errorf("the hub does not register %s, so lever cannot turn it on", conduitExperiment)
		}
		got := cur
		if len(want) > 0 {
			got, err = hc.SetExperimentOverrides(ctx, want, cur.Revision)
			if errors.Is(err, hubapi.ErrExperimentsConflict) && attempt < pinConflictRetries {
				continue
			}
			if err != nil {
				return state.HubExperimentsRecord{}, err
			}
		}
		rec := state.HubExperimentsRecord{Asked: pinnedNames(), Revision: got.Revision, PinnedAt: now}
		for _, name := range registered {
			e, ok := got.Lookup(name)
			on := wantOn(name)
			if !ok || e.Override == nil || *e.Override != on || e.Enabled != on {
				return state.HubExperimentsRecord{}, fmt.Errorf("the hub does not report %s overridden %s after the write", name, onOff(on))
			}
			if on {
				rec.On = append(rec.On, name)
			} else {
				rec.Off = append(rec.Off, name)
			}
		}
		return rec, nil
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// experimentsPin is ensureHubExperiments' seams.
type experimentsPin struct {
	// admin is the hub as lever-admin: the experiments admin route is
	// hub-admin only, and lever-admin signs every probe route, so this also
	// works on a fresh hub before the controller PAT exists.
	admin *hubapi.Client
	st    state.State
	known scopeKnownFunc
	now   func() time.Time
	// restartHub stops and starts the live hub and waits for its broker.
	restartHub func(ctx context.Context) error
	// conduitOn asks for conduit on, for `lever ssh` (sshGate.on).
	conduitOn bool
	// conduitPolicyLive returns nil only when the live hub runs with lever's
	// conduit policy (sshGate). It must be set when conduitOn is.
	conduitPolicyLive func(ctx context.Context) error
	log, warn         func(format string, args ...any)
}

// ensureHubExperiments pins scion's experiments in pinnedOffExperiments off
// as lever-admin on every apply of a scion that has them, then proves every
// probe route answers 404. With conduitOn it turns conduit on instead, but
// only on a hub that runs with lever's conduit policy, and proves that again
// after the hub restarts with conduit; anything it cannot prove leaves conduit
// off. An experiment whose state changes restarts the hub, since conduit's
// relay starts only with the hub and a pin alone would leave a running relay
// (or none). A probe that gets no answer fails the apply rather than pass as
// off. A scion that cannot say whether it has hub experiments is probed too: a
// hub without them answers 404 to every probe.
func ensureHubExperiments(ctx context.Context, p experimentsPin) (conduitOn bool, err error) {
	rec, found, err := p.st.LoadHubExperimentsRecord()
	if err != nil {
		return false, fmt.Errorf("hub experiments: %w", err)
	}
	has, sure := hubExperimentsSupport(ctx, p.known, rec, found)
	if !has && sure {
		if p.conduitOn {
			p.warn("%s: the jail's scion has no hub experiments; conduit stays off", sshOffPrefix)
		}
		return false, nil
	}
	names := strings.Join(pinnedOffNames(), ", ")
	on, err := liveExperiments(ctx, p.admin)
	if err != nil {
		return false, fmt.Errorf("hub experiments: could not tell whether %s are off: %w", names, err)
	}
	if !has {
		if p.conduitOn {
			p.warn("%s: the jail's scion did not say whether it has hub experiments; conduit stays off", sshOffPrefix)
		}
		if len(on) == 0 {
			return false, nil
		}
		return false, fmt.Errorf("hub experiments: %s is on at the hub, and lever cannot pin it off because the jail's scion did not say whether it has hub experiments (`scion hub token create --help`); "+
			"run `lever stop` and check the hub's admin experiments page before `lever up`", strings.Join(on, " and "))
	}
	conduitOn = p.conduitOn
	if conduitOn {
		if err := policyLive(ctx, p); err != nil {
			p.warn("%s: %v; conduit stays off", sshOffPrefix, err)
			conduitOn = false
		}
	}
	pinned, err := pinExperiments(ctx, p.admin, p.now(), conduitOn)
	switch {
	case errors.Is(err, errExperimentsMalformed) && len(on) == 0:
		if conduitOn {
			p.warn("%s: %v; conduit stays off", sshOffPrefix, err)
			conduitOn = false
		}
		p.warn("hub experiments: %v, so scion keeps %s off but lever cannot pin them; "+
			"scion then keeps %s off too, so the caps on remote users cannot be written or read; "+
			"reset them to the defaults in the hub's admin experiments page, then run `lever apply`", err, names, accessConstraintsExperiment)
	case err != nil:
		return false, fmt.Errorf("hub experiments: pinning %s as lever-admin: %w", names, err)
	default:
		if err := p.st.SaveHubExperimentsRecord(pinned); err != nil {
			return false, fmt.Errorf("hub experiments: persisting hub-experiments.json: %w", err)
		}
	}
	stray := strayExperiments(on, conduitOn)
	switch {
	case len(stray) > 0:
		p.log("lever: scion experiment %s was on at the hub; lever pinned it off and restarts the hub", strings.Join(stray, " and "))
	case conduitOn && !slices.Contains(on, conduitExperiment):
		p.log("lever: turned scion's %s on for `lever ssh`; restarting the hub so its relay starts", conduitExperiment)
	}
	if len(stray) > 0 || conduitOn && !slices.Contains(on, conduitExperiment) {
		if err := p.restartHub(ctx); err != nil {
			return false, fmt.Errorf("hub experiments: restarting the hub after pinning experiments: %w", err)
		}
	}
	if conduitOn {
		if err := policyLive(ctx, p); err != nil {
			return false, conduitBackOff(ctx, p, fmt.Errorf("the hub restarted without lever's conduit policy: %w", err))
		}
	}
	on, err = liveExperiments(ctx, p.admin)
	if err != nil {
		if conduitOn {
			return false, conduitBackOff(ctx, p, fmt.Errorf("could not confirm %s are as pinned: %w", names, err))
		}
		return false, fmt.Errorf("hub experiments: could not confirm %s are off: %w", names, err)
	}
	if stray := strayExperiments(on, conduitOn); len(stray) > 0 {
		if conduitOn {
			return false, conduitBackOff(ctx, p, fmt.Errorf("%s is still on at the hub after lever pinned it off", strings.Join(stray, " and ")))
		}
		return false, fmt.Errorf("hub experiments: %s is still on at the hub after lever pinned it off; "+
			"run `lever stop` and check the hub's admin experiments page before `lever up`", strings.Join(stray, " and "))
	}
	if conduitOn && !slices.Contains(on, conduitExperiment) {
		return false, conduitBackOff(ctx, p, fmt.Errorf("the hub does not serve %s after lever turned it on", conduitExperiment))
	}
	return conduitOn, nil
}

// prePinExperiments pins scion's experiments, as ensureHubExperiments
// would, on a hub that is about to restart, so the restart takes the pin and
// ensureHubExperiments then finds nothing to restart for. It keeps conduit
// on only where the hub serves it now and `lever ssh` wants it: turning it
// on needs the proof only the restarted hub can give, so that stays
// ensureHubExperiments' own restart. It writes nothing on a hub it cannot
// ask first.
func prePinExperiments(ctx context.Context, p experimentsPin) error {
	rec, found, err := p.st.LoadHubExperimentsRecord()
	if err != nil {
		return fmt.Errorf("hub experiments before the restart: %w", err)
	}
	if has, sure := hubExperimentsSupport(ctx, p.known, rec, found); !has || !sure {
		return nil
	}
	on, err := liveExperiments(ctx, p.admin)
	if err != nil {
		return fmt.Errorf("hub experiments before the restart: %w", err)
	}
	keepConduit := p.conduitOn && slices.Contains(on, conduitExperiment)
	pinned, err := pinExperiments(ctx, p.admin, p.now(), keepConduit)
	if err != nil {
		return fmt.Errorf("hub experiments before the restart: pinning as lever-admin: %w", err)
	}
	if err := p.st.SaveHubExperimentsRecord(pinned); err != nil {
		return fmt.Errorf("hub experiments before the restart: persisting hub-experiments.json: %w", err)
	}
	return nil
}

// strayExperiments are the served experiments lever does not want on.
func strayExperiments(served []string, conduitOn bool) []string {
	var out []string
	for _, n := range served {
		if !(conduitOn && n == conduitExperiment) {
			out = append(out, n)
		}
	}
	return out
}

func policyLive(ctx context.Context, p experimentsPin) error {
	if p.conduitPolicyLive == nil {
		return errors.New("lever has no way to check the hub's conduit policy")
	}
	return p.conduitPolicyLive(ctx)
}

// conduitBackOff pins conduit off again after a failed proof that it is on
// safely, restarts the hub so no relay runs, and returns cause.
func conduitBackOff(ctx context.Context, p experimentsPin, cause error) error {
	err := fmt.Errorf("hub experiments: %s: %w; lever pinned conduit off again", sshOffPrefix, cause)
	pinned, perr := pinExperimentsOff(ctx, p.admin, p.now())
	if perr != nil {
		return fmt.Errorf("%w, but pinning it off failed (%v): run `lever stop` before anything else", err, perr)
	}
	if serr := p.st.SaveHubExperimentsRecord(pinned); serr != nil {
		return fmt.Errorf("%w; persisting hub-experiments.json: %v", err, serr)
	}
	if rerr := p.restartHub(ctx); rerr != nil {
		return fmt.Errorf("%w, but the hub restart failed (%v): run `lever stop` before anything else", err, rerr)
	}
	return err
}
