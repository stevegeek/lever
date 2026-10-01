package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// managerBootstrapPath is where the manager's own bootstrap.json is readable
// from inside the manager CONTAINER, where scion mounts the tree at /workspace
// (the jail-level /lever mount does not exist in the container), so the
// bootstrap deposited by `lever apply` at <tree>/.lever/bootstrap.json appears
// here.
const managerBootstrapPath = "/workspace/.lever/bootstrap.json"

// managerIDDir is the directory holding the manager's mTLS identity
// (cert+key+ca): "~/.lever-id" for the process user.
func managerIDDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lever-id")
}

// workerResult is the CLI's merged decode target across ALL worker endpoints:
// the single-worker verbs return {worker, phase} (wire.WorkerResponse) and
// /worker/list returns {agents} (wire.WorkerListResponse). Embedding both
// wire types (their json-tagged fields promote through the anonymous embeds)
// sources every field from the one declaration rather than a re-typed copy,
// while keeping one decode type for the generic brokerCall.
type workerResult struct {
	wire.WorkerResponse
	wire.WorkerListResponse[scion.Agent]
}

// brokerCaller is what every manager subcommand drives the broker through:
// POST body as JSON to one broker endpoint and decode the reply into out.
// The production implementation is mtlsCaller; tests substitute an httpCaller
// aimed at an httptest server.
type brokerCaller interface {
	Call(ctx context.Context, endpoint string, body, out any) error
}

// httpCaller posts to baseURL+endpoint with client. It is the transport half
// of mtlsCaller and the whole of a test double.
type httpCaller struct {
	client  *http.Client
	baseURL string
}

func (c httpCaller) Call(ctx context.Context, endpoint string, body, out any) error {
	return httpjson.Post(ctx, c.client, c.baseURL+endpoint, body, out)
}

// bootstrapEnv names an agent's bootstrap.json when it is not at the manager's
// path: the broker sets it on every worker to the ticket it mounts read-only
// at /run/lever (broker.workerTicketEnv), and `lever-agent` reads the same
// variable.
const bootstrapEnv = "LEVER_BOOTSTRAP"

// bootstrapPath is the bootstrap.json this agent's broker URL comes from:
// $LEVER_BOOTSTRAP when set (a worker), else the manager's path. A worker has
// no bootstrap.json in its tree; reading the manager's path there fails, or
// worse, finds a stale copy someone left in the agent-writable tree.
func bootstrapPath() string {
	if p := os.Getenv(bootstrapEnv); p != "" {
		return p
	}
	return managerBootstrapPath
}

// mtlsCaller builds the agent's mTLS client from its bootstrap + identity on
// every call and POSTs through it. The same binary serves the manager and the
// workers: bootstrapPath picks the bootstrap, and the identity directory is
// the process user's in both.
//
// gatewayURL, when set, is the agent's loopback gateway (`lever-agent
// gateway`), which forwards any path to the broker with the agent's own
// identity. It is the fallback when the bootstrap cannot be read at all (a
// ticket mounted with an owner this user is not): the gateway learned the
// broker URL at boot, so the call needs neither file.
type mtlsCaller struct {
	bootstrapPath string
	idDir         string
	gatewayURL    string
}

func newMTLSCaller() mtlsCaller {
	return mtlsCaller{bootstrapPath: bootstrapPath(), idDir: managerIDDir(), gatewayURL: agent.LocalGatewayURL}
}

func (c mtlsCaller) Call(ctx context.Context, endpoint string, body, out any) error {
	bs, err := agent.LoadBootstrap(c.bootstrapPath)
	if err != nil {
		unreadable := errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
		if c.gatewayURL == "" || !unreadable {
			return fmt.Errorf("bootstrap %s: %w", c.bootstrapPath, err)
		}
		gerr := httpCaller{client: gatewayClient, baseURL: c.gatewayURL}.Call(ctx, endpoint, body, out)
		if gerr == nil {
			return nil
		}
		if httpjson.Status(gerr) != 0 {
			// An HTTP answer through the gateway stands, a broker refusal
			// included. The wrap keeps the status and says which way the
			// call went: a bare 502 here is the gateway failing to reach
			// the broker.
			return fmt.Errorf("via the agent gateway: %w", gerr)
		}
		return fmt.Errorf("bootstrap %s: %w; and the agent gateway at %s did not answer: %v", c.bootstrapPath, err, c.gatewayURL, gerr)
	}
	id, ok := agent.LoadIdentity(c.idDir)
	if !ok {
		return fmt.Errorf("agent identity not found in %s", c.idDir)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("agent mTLS client: %w", err)
	}
	return httpCaller{client: client, baseURL: bs.BrokerURL}.Call(ctx, endpoint, body, out)
}

// gatewayClient talks plaintext HTTP to the loopback gateway. No timeout of
// its own: the caller's ctx bounds the call, as it does on the mTLS path.
var gatewayClient = &http.Client{}

// brokerCall is c.Call with a typed return: the decoded T, or the zero T on
// error.
func brokerCall[T any](ctx context.Context, c brokerCaller, endpoint string, body any) (T, error) {
	var res T
	if err := c.Call(ctx, endpoint, body, &res); err != nil {
		var zero T
		return zero, err
	}
	return res, nil
}
