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

// managerBootstrapPath is inside the manager container, where scion mounts the
// tree at /workspace; the jail-level /lever mount does not exist there.
const managerBootstrapPath = "/workspace/.lever/bootstrap.json"

func managerIDDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lever-id")
}

// workerResult is one decode target for every worker endpoint: it embeds
// wire.WorkerResponse and wire.WorkerListResponse.
type workerResult struct {
	wire.WorkerResponse
	wire.WorkerListResponse[scion.Agent]
}

// brokerCaller POSTs body as JSON to one broker endpoint and decodes the reply
// into out.
type brokerCaller interface {
	Call(ctx context.Context, endpoint string, body, out any) error
}

type httpCaller struct {
	client  *http.Client
	baseURL string
}

func (c httpCaller) Call(ctx context.Context, endpoint string, body, out any) error {
	return httpjson.Post(ctx, c.client, c.baseURL+endpoint, body, out)
}

// bootstrapEnv names a worker's bootstrap.json: the broker sets it to the ticket
// it mounts read-only at /run/lever (broker.workerTicketEnv).
const bootstrapEnv = "LEVER_BOOTSTRAP"

// bootstrapPath is $LEVER_BOOTSTRAP when set (a worker), else the manager's
// path. A worker must not read the manager's path: it can hold a stale copy in
// the agent-writable tree.
func bootstrapPath() string {
	if p := os.Getenv(bootstrapEnv); p != "" {
		return p
	}
	return managerBootstrapPath
}

// mtlsCaller builds the agent's mTLS client from its bootstrap and identity on
// every call; it serves both the manager and workers.
//
// gatewayURL, when set, is the agent's loopback gateway (`lever-agent
// gateway`), which forwards to the broker with the agent's own identity. It is
// the fallback only when the bootstrap cannot be read at all (a ticket owned
// by another user).
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
			// A broker answer through the gateway stands, a refusal included. A bare 502
			// here means the gateway could not reach the broker.
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

// No timeout of its own: the caller's ctx bounds the call.
var gatewayClient = &http.Client{}

func brokerCall[T any](ctx context.Context, c brokerCaller, endpoint string, body any) (T, error) {
	var res T
	if err := c.Call(ctx, endpoint, body, &res); err != nil {
		var zero T
		return zero, err
	}
	return res, nil
}
