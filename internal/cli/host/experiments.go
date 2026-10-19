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
// links beside lever's reviewed file paths. hub.chat_thread_groups lets agents
// create and rename project-wide chat groups that project members with read
// access see, and copies users' personal groups into the project. federation.project_visitors
// admits users of external issuers and mints this hub's users credentials for
// peer hubs; its only route is POST and also needs the hub's OIDC provider,
// which lever does not configure, so no GET tells whether it is on. scion reads
// all of them per request from the hub database, except conduit's relay,
// which starts only with the hub.
var pinnedOffExperiments = []pinnedExperiment{
	{name: "hub.conduit", probe: "/api/v1/conduit/grant-keys"},
	{name: "hub.artifacts", probe: "/api/v1/artifacts?mine=1"},
	{name: "hub.chat_thread_groups", probe: "/api/v1/thread-groups"},
	{name: "federation.project_visitors"},
}

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

// pinExperimentsOff is pinExperiments with every experiment off.
func pinExperimentsOff(ctx context.Context, hc *hubapi.Client, now time.Time) (state.HubExperimentsRecord, error) {
	return pinExperiments(ctx, hc, now, false)
}

// pinExperiments writes an explicit override for each pinned experiment the
// hub registers and does not already hold at the wanted value (false, or true
// for conduit when conduitOn), leaving every other override as it is, then
// requires the hub's answer to show each one overridden that way.
func pinExperiments(ctx context.Context, hc *hubapi.Client, now time.Time, conduitOn bool) (state.HubExperimentsRecord, error) {
	wantOn := func(name string) bool { return conduitOn && name == conduitExperiment }
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
		for _, name := range pinnedOffNames() {
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
		rec := state.HubExperimentsRecord{Asked: pinnedOffNames(), Revision: got.Revision, PinnedAt: now}
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
			"reset them to the defaults in the hub's admin experiments page, then run `lever apply`", err, names)
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
