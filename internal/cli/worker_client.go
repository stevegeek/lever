package cli

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// workerResult is the CLI's merged decode target across ALL worker endpoints:
// the single-worker verbs return {worker, phase} (wire.WorkerResponse) and
// /worker/list returns {agents} (broker.WorkerListResponse). Embedding both
// wire types (their json-tagged fields promote through the anonymous embeds)
// sources every field from the one declaration rather than a re-typed copy,
// while keeping one decode type for the generic postBroker.
type workerResult struct {
	wire.WorkerResponse
	broker.WorkerListResponse
}

// postBroker POSTs body as JSON to baseURL+endpoint using client, decoding the
// response into T (httpjson.Post with a typed return). Split out for
// unit-testing without mTLS; workerCall specializes it to the worker-command
// response shape.
func postBroker[T any](ctx context.Context, client *http.Client, baseURL, endpoint string, body any) (T, error) {
	var res T
	if err := httpjson.Post(ctx, client, baseURL+endpoint, body, &res); err != nil {
		var zero T
		return zero, err
	}
	return res, nil
}

// brokerCall builds the manager's mTLS client from its bootstrap + identity and
// POSTs endpoint, decoding into T. This is the production entry both the agent
// (worker) and msg/watch subcommands use — the bootstrap/identity paths are
// agent-generic (workers get their own bootstrap at the same in-container path,
// so the same binary works for manager AND workers).
//
// brokerCall is generic, so it cannot be assigned directly to a package-level
// seam var (`var xCallFn = brokerCall` doesn't type-check without explicit
// instantiation). Each call site instead gets a small concrete wrapper
// (workerCall, msgCall) that instantiates brokerCall for its response type;
// the wrapper is what the test seam (workerCallFn, msgCallFn) points at.
func brokerCall[T any](ctx context.Context, endpoint string, body any) (T, error) {
	var zero T
	bs, err := agent.LoadBootstrap(managerBootstrapPath)
	if err != nil {
		return zero, fmt.Errorf("manager bootstrap: %w", err)
	}
	id, ok := agent.LoadIdentity(managerIDDir)
	if !ok {
		return zero, fmt.Errorf("manager identity not found in %s", managerIDDir)
	}
	client, err := id.Client()
	if err != nil {
		return zero, fmt.Errorf("manager mTLS client: %w", err)
	}
	return postBroker[T](ctx, client, bs.BrokerURL, endpoint, body)
}

// workerCall is brokerCall specialized to the worker-command response shape.
// This is the production entry the agent subcommands use (via workerCallFn).
func workerCall(ctx context.Context, endpoint string, body any) (workerResult, error) {
	return brokerCall[workerResult](ctx, endpoint, body)
}
