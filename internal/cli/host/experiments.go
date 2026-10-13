package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// pinnedExperiment is a scion hub experiment lever keeps off, with a read-only
// route the hub answers 404 exactly while the experiment is off. An older scion
// lacks the route and answers 404 too, which is also "off". The controller PAT
// can read both routes; the admin view cannot be read without hub-admin.
type pinnedExperiment struct {
	name  string
	probe string
}

// pinnedOffExperiments are the experiments lever pins off. hub.conduit adds an
// inbound shell, sftp and port path into every agent, reaching the jail
// loopback and any Unix socket; hub.artifacts adds hub-held publishing with
// unauthenticated share links beside lever's reviewed file paths. scion reads
// both per request from the hub database, except conduit's relay, which starts
// only with the hub.
var pinnedOffExperiments = []pinnedExperiment{
	{name: "hub.conduit", probe: "/api/v1/conduit/grant-keys"},
	{name: "hub.artifacts", probe: "/api/v1/artifacts?mine=1"},
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
	if known != nil {
		if has, err := known(ctx, experimentsScope); err == nil {
			return has
		}
	}
	return found && !rec.NotApplicable
}

// liveExperiments returns the pinned-off experiments the hub serves now. Any
// answer but 404 counts as served: scion gates each probe route on its
// experiment before anything else can refuse the request.
func liveExperiments(ctx context.Context, hc *hubapi.Client) ([]string, error) {
	var live []string
	for _, e := range pinnedOffExperiments {
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

// experimentsCheck is the bootstrap-token step's view of the pin. has: every
// window pins. reason: a window is wanted. urgent: a pinned-off experiment is
// live, so apply must not go on without the window.
type experimentsCheck struct {
	has    bool
	reason string
	urgent bool
}

// checkHubExperimentsForApply probes the live hub with the controller PAT. A
// hub that is down or refuses the PAT tells nothing here: the check after the
// hub starts asks again (ensureHubExperimentsOff).
func checkHubExperimentsForApply(ctx context.Context, live *hubapi.Client, st state.State, known scopeKnownFunc) (experimentsCheck, error) {
	rec, found, err := st.LoadHubExperimentsRecord()
	if err != nil {
		return experimentsCheck{}, err
	}
	if !hubHasExperiments(ctx, known, rec, found) {
		return experimentsCheck{}, nil
	}
	c := experimentsCheck{has: true}
	if on, err := liveExperiments(ctx, live); err == nil && len(on) > 0 {
		c.reason = fmt.Sprintf("scion experiment %s is on at the hub", strings.Join(on, " and "))
		c.urgent = true
		return c, nil
	}
	if !found || rec.NotApplicable {
		c.reason = "scion's " + strings.Join(pinnedOffNames(), " and ") + " experiments are not pinned off on this hub yet"
	}
	return c, nil
}

// experimentsWindowBlocked is the apply failure for a live pinned-off
// experiment beside running containers.
func experimentsWindowBlocked(reason, blocked string) error {
	return fmt.Errorf("bootstrap-token: %s and lever must pin it off, but the dev-auth window cannot open: %s. "+
		"Agents can reach the jail loopback, and the window's dev-auth hub would hand them admin-grade tokens. "+
		"Run `lever stop`, then `lever up` (the manager conversation is kept): the window then pins it off before the hub starts", reason, blocked)
}

// pinHubExperimentsOff runs in the dev-auth window as the dev identity. A
// failure fails the window: lever does not run beside an experiment it could
// not pin. A malformed store only warns, since scion then resolves every
// server-layer experiment off and refuses writes until an admin resets it.
func pinHubExperimentsOff(ctx context.Context, jr proc.Runner, st state.State, now time.Time, o patMintOpts) error {
	hub, err := windowAdminHub(ctx, jr, o)
	if err != nil {
		return fmt.Errorf("bootstrap-token: pinning scion experiments off: %w", err)
	}
	rec, err := pinExperimentsOff(ctx, &hubapi.Client{T: hub}, now)
	if errors.Is(err, errExperimentsMalformed) {
		o.warn("bootstrap-token: %v, so scion keeps %s off but lever cannot pin them; "+
			"reset them to the defaults in the hub's admin experiments page, then run `lever apply`", err, strings.Join(pinnedOffNames(), " and "))
		return nil
	}
	if err != nil {
		return fmt.Errorf("bootstrap-token: pinning scion experiments off: %w", err)
	}
	if err := st.SaveHubExperimentsRecord(rec); err != nil {
		return fmt.Errorf("bootstrap-token: persisting hub-experiments.json: %w", err)
	}
	return nil
}

// pinConflictRetries bounds re-reads after a concurrent write to the
// experiments row.
const pinConflictRetries = 3

// pinExperimentsOff writes an explicit false for each pinned-off experiment the
// hub registers and does not already hold at false, leaving every other
// override as it is, then requires the hub's answer to show each one
// overridden off and disabled.
func pinExperimentsOff(ctx context.Context, hc *hubapi.Client, now time.Time) (state.HubExperimentsRecord, error) {
	for attempt := 1; ; attempt++ {
		cur, err := hc.AdminExperiments(ctx)
		if errors.Is(err, hubapi.ErrNoExperiments) {
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
			if e.Override == nil || *e.Override {
				want[name] = false
			}
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
		for _, name := range registered {
			e, ok := got.Lookup(name)
			if !ok || e.Override == nil || *e.Override || e.Enabled {
				return state.HubExperimentsRecord{}, fmt.Errorf("the hub does not report %s overridden off after the write", name)
			}
		}
		return state.HubExperimentsRecord{Off: registered, Revision: got.Revision, PinnedAt: now}, nil
	}
}

// experimentsRepair is ensureHubExperimentsOff's seams.
type experimentsRepair struct {
	live  *hubapi.Client
	st    state.State
	known scopeKnownFunc
	// window runs the bootstrap-token step, which opens the dev-auth window for
	// a live pinned-off experiment or fails with the repair.
	window func(ctx context.Context) error
	// startHub starts the live hub with its normal options.
	startHub func(ctx context.Context) error
	log      func(format string, args ...any)
}

// ensureHubExperimentsOff runs right after the scion-server step starts the
// hub. The bootstrap-token step cannot probe a hub that was down when it ran
// (after `lever stop`, a reboot), and conduit's relay starts only with the hub,
// so a hub that came up with a pinned-off experiment on is caught here: the
// window pins it off and the hub starts again without it, or apply fails.
//
// The hub is up and the controller PAT is fresh here, so a probe that gets no
// answer fails the apply rather than pass as off.
func ensureHubExperimentsOff(ctx context.Context, r experimentsRepair) error {
	rec, found, err := r.st.LoadHubExperimentsRecord()
	if err != nil {
		return fmt.Errorf("hub experiments: %w", err)
	}
	if !hubHasExperiments(ctx, r.known, rec, found) {
		return nil
	}
	on, err := liveExperiments(ctx, r.live)
	if err != nil {
		return fmt.Errorf("hub experiments: could not tell whether %s are off: %w", strings.Join(pinnedOffNames(), " and "), err)
	}
	if len(on) == 0 {
		return nil
	}
	r.log("lever: scion experiment %s is on at the hub; pinning it off in the dev-auth window and restarting the hub", strings.Join(on, " and "))
	if err := r.window(ctx); err != nil {
		return err
	}
	// The window leaves the live hub stopped when it succeeds.
	if err := r.startHub(ctx); err != nil {
		return fmt.Errorf("hub experiments: starting the hub after pinning experiments off: %w", err)
	}
	on, err = liveExperiments(ctx, r.live)
	if err != nil {
		return fmt.Errorf("hub experiments: could not confirm %s are off after the restart: %w", strings.Join(pinnedOffNames(), " and "), err)
	}
	if len(on) > 0 {
		return fmt.Errorf("hub experiments: %s is still on at the hub after lever pinned it off; "+
			"run `lever stop` and check the hub's admin experiments page before `lever up`", strings.Join(on, " and "))
	}
	return nil
}

// ensureHubExperimentsOff backs Deps.EnsureHubExperimentsOff.
func (w *applyWiring) ensureHubExperimentsOff(ctx context.Context) error {
	return ensureHubExperimentsOff(ctx, experimentsRepair{
		live:   w.hub(),
		st:     w.state,
		known:  w.sc.KnowsUATScope,
		window: w.ensureControllerPAT,
		startHub: func(ctx context.Context) error {
			opts := apply.HubServerOpts(w.app, w.deps.HubSessionSecret)
			opts.Progress = w.deps.Log
			return w.sc.ServerStart(ctx, opts)
		},
		log: w.log,
	})
}
