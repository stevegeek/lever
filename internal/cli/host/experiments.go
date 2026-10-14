package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/state"
)

// pinnedExperiment is a scion hub experiment lever keeps off, with a read-only
// route the hub answers 404 exactly while the experiment is off. An older scion
// lacks the route and answers 404 too, which is also "off". The controller PAT
// and lever-admin can read both routes; only lever-admin reads the admin view.
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

// experimentsPin is ensureHubExperimentsOff's seams.
type experimentsPin struct {
	// admin is the hub as lever-admin: the experiments admin route is
	// hub-admin only, and the probes need no token, so this also works on a
	// fresh hub before the controller PAT exists.
	admin *hubapi.Client
	st    state.State
	known scopeKnownFunc
	now   func() time.Time
	// restartHub stops and starts the live hub and waits for its broker.
	restartHub func(ctx context.Context) error
	log, warn  func(format string, args ...any)
}

// ensureHubExperimentsOff pins scion's conduit and artifacts experiments off
// as lever-admin on every apply of a scion that has them, then proves both
// probe routes answer 404. An experiment that was on before the pin restarts
// the hub, since conduit's relay starts only with the hub and a pin alone
// would leave a running relay. A probe that gets no answer fails the apply
// rather than pass as off. A scion that cannot say whether it has hub
// experiments is probed too: a hub without them answers 404 to both probes.
func ensureHubExperimentsOff(ctx context.Context, p experimentsPin) error {
	rec, found, err := p.st.LoadHubExperimentsRecord()
	if err != nil {
		return fmt.Errorf("hub experiments: %w", err)
	}
	has, sure := hubExperimentsSupport(ctx, p.known, rec, found)
	if !has && sure {
		return nil
	}
	names := strings.Join(pinnedOffNames(), " and ")
	on, err := liveExperiments(ctx, p.admin)
	if err != nil {
		return fmt.Errorf("hub experiments: could not tell whether %s are off: %w", names, err)
	}
	if !has {
		if len(on) == 0 {
			return nil
		}
		return fmt.Errorf("hub experiments: %s is on at the hub, and lever cannot pin it off because the jail's scion did not say whether it has hub experiments (`scion hub token create --help`); "+
			"run `lever stop` and check the hub's admin experiments page before `lever up`", strings.Join(on, " and "))
	}
	pinned, err := pinExperimentsOff(ctx, p.admin, p.now())
	switch {
	case errors.Is(err, errExperimentsMalformed) && len(on) == 0:
		p.warn("hub experiments: %v, so scion keeps %s off but lever cannot pin them; "+
			"reset them to the defaults in the hub's admin experiments page, then run `lever apply`", err, names)
	case err != nil:
		return fmt.Errorf("hub experiments: pinning %s off as lever-admin: %w", names, err)
	default:
		if err := p.st.SaveHubExperimentsRecord(pinned); err != nil {
			return fmt.Errorf("hub experiments: persisting hub-experiments.json: %w", err)
		}
	}
	if len(on) > 0 {
		p.log("lever: scion experiment %s was on at the hub; lever pinned it off and restarts the hub", strings.Join(on, " and "))
		if err := p.restartHub(ctx); err != nil {
			return fmt.Errorf("hub experiments: restarting the hub after pinning experiments off: %w", err)
		}
	}
	on, err = liveExperiments(ctx, p.admin)
	if err != nil {
		return fmt.Errorf("hub experiments: could not confirm %s are off: %w", names, err)
	}
	if len(on) > 0 {
		return fmt.Errorf("hub experiments: %s is still on at the hub after lever pinned it off; "+
			"run `lever stop` and check the hub's admin experiments page before `lever up`", strings.Join(on, " and "))
	}
	return nil
}
