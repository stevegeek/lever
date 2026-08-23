package manager

import (
	"context"
	"fmt"
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

// mtlsCaller builds the manager's mTLS client from its bootstrap + identity on
// every call and POSTs through it. The bootstrap/identity paths are
// agent-generic (workers get their own bootstrap at the same in-container
// path, so the same binary works for manager AND workers).
type mtlsCaller struct {
	bootstrapPath string
	idDir         string
}

func newMTLSCaller() mtlsCaller {
	return mtlsCaller{bootstrapPath: managerBootstrapPath, idDir: managerIDDir()}
}

func (c mtlsCaller) Call(ctx context.Context, endpoint string, body, out any) error {
	bs, err := agent.LoadBootstrap(c.bootstrapPath)
	if err != nil {
		return fmt.Errorf("manager bootstrap: %w", err)
	}
	id, ok := agent.LoadIdentity(c.idDir)
	if !ok {
		return fmt.Errorf("manager identity not found in %s", c.idDir)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("manager mTLS client: %w", err)
	}
	return httpCaller{client: client, baseURL: bs.BrokerURL}.Call(ctx, endpoint, body, out)
}

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
