package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// Which instance a broker belongs to (lever#181). A broker's ports say
// nothing about that: two instances on one host can be configured with the
// same ports, and then whichever broker bound them first answers both. Every
// host-side caller of the admin port therefore reads the broker's instance
// id from /epoch and compares it with this instance's (state.InstanceIDFile)
// before it trusts, reuses, or acts on that broker.

// brokerAction is what apply's broker start does with a broker already
// answering on the admin port.
type brokerAction int

const (
	brokerReuse   brokerAction = iota // this instance's, same binary and config
	brokerRestart                     // this instance's (or, predating ids, very likely ours): stop it, then start
	brokerRefuse                      // not this instance's: touch nothing and fail
)

// judgeRunningBroker decides what to do with a broker that answered /epoch.
// ours is this instance's id. ownPIDAlive is whether this instance's
// broker.pid names a live process.
//
//   - Same id: reuse it when version and config hash match, else restart it.
//   - Another id: it is another instance's broker. Refuse, and stop nothing.
//   - No id: a broker started by a lever that predates instance ids. Its version
//     differs from this binary's, so it would be restarted anyway. Restart
//     it only when this instance's pid file names a live process, which is
//     what StopBroker stops; the caller then checks the port fell silent,
//     so a broker that was not the one stopped is never adopted. Without a
//     live pid nothing ties that broker to this instance, so refuse.
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

// shortID shortens an instance id for messages. Not a secret; just long.
func shortID(id string) string {
	if id == "" {
		return "none"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// foreignBrokerError explains a broker on this instance's admin port that
// is not this instance's.
func foreignBrokerError(adminPort int, got broker.EpochAnswer, ours string) error {
	if got.InstanceID == "" {
		return fmt.Errorf("a lever broker that reports no instance id (version %q, an older lever) is serving on 127.0.0.1:%d, "+
			"and this instance's %s names no live process, so nothing shows it is this instance's broker; lever leaves it running. "+
			"If it is another instance's, give this instance its own broker.jail_port and broker.admin_port; if it is this instance's, stop that process, then run this again",
			got.Version, adminPort, "broker.pid")
	}
	return fmt.Errorf("the broker on 127.0.0.1:%d belongs to another lever instance (its instance id %s, this instance's %s); lever leaves it running. "+
		"Give this instance its own broker.jail_port and broker.admin_port (and remote.port, remote.login_port and tool backend ports, see the operations guide). "+
		"If it is this instance's own broker from before its directory was moved or copied, stop that broker process first",
		adminPort, shortID(got.InstanceID), shortID(ours))
}

// errNoBroker means nothing answered /epoch as a lever broker.
var errNoBroker = errors.New("no lever broker answers on the admin port")

// readBrokerIdentity GETs /epoch on adminURL. errNoBroker when nothing
// answers with a decodable 200.
func readBrokerIdentity(ctx context.Context, client *http.Client, adminURL string) (broker.EpochAnswer, error) {
	var got broker.EpochAnswer
	if err := httpjson.Get(ctx, client, adminURL+wire.PathEpoch, &got); err != nil {
		return broker.EpochAnswer{}, fmt.Errorf("%w: %v", errNoBroker, err)
	}
	return got, nil
}

// requireOwnBroker fails unless the broker answering on adminPort is this
// instance's: it reports an id, and the id is the one in st. For the
// operator commands that act on a running broker (revoke, bump-epoch):
// sent to another instance's broker they would revoke that instance's
// agents. A broker predating instance ids is refused too, with the way out.
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
