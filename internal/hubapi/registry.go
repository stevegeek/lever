package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

const systemRegistryPath = "/api/v1/system/registry"

const registryConflictRetries = 3

// SetImageRegistry sets the hub's agent image registry prefix
// (PUT /api/v1/system/registry). The hub answers with the request's value, so
// the check on the answer catches a changed API, not a lost write. A 409 (a
// concurrent write to the same settings row) is retried a few times.
//
// The route takes any loopback caller, an agent included. A hub with
// DB-backed operational settings stores the value in its endpoints section,
// which outranks settings.yaml and applies at the next agent dispatch without
// a restart, so `scion config set --global image_registry` cannot undo an
// agent's write; this route can.
func (c *Client) SetImageRegistry(ctx context.Context, registry string) error {
	if registry == "" {
		return errors.New("hubapi: setting the image registry needs a value")
	}
	var out struct {
		ImageRegistry *string `json:"image_registry"`
	}
	var err error
	for attempt := 0; attempt < registryConflictRetries; attempt++ {
		err = c.send(ctx, http.MethodPut, systemRegistryPath, struct {
			ImageRegistry string `json:"image_registry"`
		}{registry}, &out)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
			break
		}
	}
	if err != nil {
		return err
	}
	if out.ImageRegistry == nil || *out.ImageRegistry != registry {
		return &APIError{Msg: fmt.Sprintf("PUT %s: the hub did not echo image_registry %q; the hub API changed shape", systemRegistryPath, registry)}
	}
	return nil
}

const systemRuntimePath = "/api/v1/system/runtime"

// systemRuntime is the hub's answer on the runtime route; Configured is the
// active profile's runtime, "" when none is set.
type systemRuntime struct {
	Configured *string `json:"configured"`
}

// SetRuntime sets the container runtime of the hub's active profile
// (PUT /api/v1/system/runtime), retrying a 409 like SetImageRegistry.
//
// The route takes any loopback caller, an agent included. A hub with
// DB-backed operational settings stores the value in its profiles section,
// which outranks settings.yaml, and swaps the co-located broker's runtime
// live; a swap to the runtime already in use is a no-op.
func (c *Client) SetRuntime(ctx context.Context, rt string) error {
	if rt == "" {
		return errors.New("hubapi: setting the runtime needs a value")
	}
	var out systemRuntime
	var err error
	for attempt := 0; attempt < registryConflictRetries; attempt++ {
		err = c.send(ctx, http.MethodPut, systemRuntimePath, struct {
			Runtime string `json:"runtime"`
		}{rt}, &out)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
			break
		}
	}
	if err != nil {
		return err
	}
	if out.Configured == nil || *out.Configured != rt {
		return &APIError{Msg: fmt.Sprintf("PUT %s: the hub did not echo runtime %q; the hub API changed shape", systemRuntimePath, rt)}
	}
	return nil
}

// Runtime returns the container runtime of the hub's active profile, "" when
// none is configured.
func (c *Client) Runtime(ctx context.Context) (string, error) {
	var out systemRuntime
	if err := c.get(ctx, systemRuntimePath, &out); err != nil {
		return "", err
	}
	if out.Configured == nil {
		return "", &APIError{Msg: fmt.Sprintf("GET %s: no configured field; the hub API changed shape", systemRuntimePath)}
	}
	return *out.Configured, nil
}
