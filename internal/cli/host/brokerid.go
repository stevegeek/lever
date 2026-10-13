package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// A broker's ports do not say which instance it belongs to: two instances on
// one host can share ports, and whichever broker bound them first answers
// both. Every host-side caller of the admin port therefore compares the
// broker's /epoch instance id with this instance's (state.InstanceIDFile)
// before it trusts, reuses or acts on that broker.

type brokerAction int

const (
	brokerReuse   brokerAction = iota // this instance's, same binary and config
	brokerRestart                     // this instance's (or, predating ids, very likely ours): stop it, then start
	brokerRefuse                      // not this instance's: touch nothing and fail
)

// judgeRunningBroker decides what to do with a broker that answered /epoch.
// ownPIDAlive is whether broker.pid names a live `lever broker serve` of this
// instance (state.State.BrokerStatus; a reused pid does not count).
//
//   - Same id: reuse it when version and config hash match, else restart it.
//   - Another id: another instance's broker. Refuse and stop nothing.
//   - No id: a broker from before instance ids. Restart it only when this
//     instance's pid file names a live process, which is what StopBroker
//     stops; the caller then checks the port fell silent, so a broker that
//     was not the one stopped is never adopted. Otherwise refuse.
func judgeRunningBroker(got broker.EpochAnswer, ours, wantVersion, wantHash string, ownPIDAlive bool) brokerAction {
	switch {
	case ours != "" && got.InstanceID == ours:
		if brokerReusable(got.EpochResponse, wantVersion, wantHash) {
			return brokerReuse
		}
		return brokerRestart
	case got.InstanceID != "":
		return brokerRefuse
	case ownPIDAlive:
		return brokerRestart
	default:
		return brokerRefuse
	}
}

// heldBrokerErr is nil only when got is the broker this apply runs: this
// instance's id, version and config hash. It shares judgeRunningBroker with
// the reuse decision; ownPIDAlive is false on purpose, since a broker without
// an id is never the one this binary started.
func heldBrokerErr(got broker.EpochAnswer, ours, wantVersion, wantHash string, adminPort int) error {
	switch judgeRunningBroker(got, ours, wantVersion, wantHash, false) {
	case brokerReuse:
		return nil
	case brokerRestart: // this instance's id, another binary or config
		return fmt.Errorf("this instance's broker answers on 127.0.0.1:%d as version %q, or with another broker config, not as the broker this apply runs (version %q): "+
			"another lever apply or reload of this instance may have replaced it; lever does not use it. Run lever stop, then run this again",
			adminPort, got.Version, wantVersion)
	default:
		return foreignBrokerError(adminPort, got, ours)
	}
}

func shortID(id string) string {
	if id == "" {
		return "none"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func foreignBrokerError(adminPort int, got broker.EpochAnswer, ours string) error {
	if got.InstanceID == "" {
		return fmt.Errorf("a lever broker that reports no instance id (version %q, an older lever) is serving on 127.0.0.1:%d, "+
			"and nothing shows it is this instance's broker; lever leaves it running. "+
			"If it is another instance's, give this instance its own broker.jail_port and broker.admin_port; if it is this instance's, stop that process, then run this again",
			got.Version, adminPort)
	}
	return fmt.Errorf("the broker on 127.0.0.1:%d belongs to another lever instance (its instance id %s, this instance's %s); lever leaves it running. "+
		"Give this instance its own broker.jail_port and broker.admin_port (and remote.port, remote.login_port and tool backend ports, see the operations guide). "+
		"If it is this instance's own broker from before its directory was moved or copied, stop that broker process first",
		adminPort, shortID(got.InstanceID), shortID(ours))
}

// errNoBroker means nothing answered /epoch as a lever broker.
var errNoBroker = errors.New("no lever broker answers on the admin port")

// readBrokerIdentity returns errNoBroker when nothing answers with a decodable
// 200.
func readBrokerIdentity(ctx context.Context, client *http.Client, adminURL string) (broker.EpochAnswer, error) {
	var got broker.EpochAnswer
	if err := httpjson.Get(ctx, client, adminURL+wire.PathEpoch, &got); err != nil {
		return broker.EpochAnswer{}, fmt.Errorf("%w: %v", errNoBroker, err)
	}
	return got, nil
}

// requireOwnBroker fails unless the broker on adminPort reports this
// instance's id. Operator commands that act on a broker (revoke, bump-epoch)
// would otherwise revoke another instance's agents. A broker without an id is
// refused too, with the way out.
func requireOwnBroker(ctx context.Context, client *http.Client, adminPort int, st state.State) error {
	got, err := readBrokerIdentity(ctx, client, fmt.Sprintf("http://127.0.0.1:%d", adminPort))
	if err != nil {
		return fmt.Errorf("contacting broker admin (is this instance's broker running?): %w", err)
	}
	ours, err := st.ReadInstanceID()
	switch {
	case errors.Is(err, state.ErrNoInstanceID):
		ours = ""
	case err != nil:
		return err
	}
	if got.InstanceID == "" {
		return fmt.Errorf("the broker on 127.0.0.1:%d reports no instance id (an older lever started it): run `lever apply` to restart it with this binary first", adminPort)
	}
	if ours == "" || got.InstanceID != ours {
		return foreignBrokerError(adminPort, got, ours)
	}
	return nil
}

// brokerQuietWait bounds Start's wait for the admin port to fall silent after
// the stop. A var so tests can shorten it.
var brokerQuietWait = 3 * time.Second

func (bc *brokerController) stillAnswering(ctx context.Context, wait time.Duration) (broker.EpochAnswer, bool) {
	deadline := time.Now().Add(wait)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		got, err := readBrokerIdentity(probeCtx, bc.admin, bc.adminURL)
		cancel()
		if err != nil {
			return broker.EpochAnswer{}, false
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return got, true
		}
		select {
		case <-ctx.Done():
			return got, true
		case <-time.After(brokerHealthInterval):
		}
	}
}
