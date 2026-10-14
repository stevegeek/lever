package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const systemRegistryPath = "/api/v1/system/registry"

const registryConflictRetries = 3

// SetImageRegistry sets the hub's agent image registry prefix
// (PUT /api/v1/system/registry). The hub answers with the request's value, so
// the check on the answer catches a changed API, not a lost write. A 409 (a
// concurrent write to the same settings row) is retried a few times, and so
// is a request that got no answer (the PUT is idempotent).
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
	err := retryUnanswered(ctx, func() error {
		return c.putSetting(ctx, systemRegistryPath, struct {
			ImageRegistry string `json:"image_registry"`
		}{registry}, &out)
	}, nil)
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
// (PUT /api/v1/system/runtime), retrying like SetImageRegistry. A runtime the
// hub already has is not written again: each PUT reloads the co-located
// broker's runtime, which right after a hub start can outlast the request.
// A PUT that got no answer may still have applied, so the hub is read back
// before it is sent again.
//
// The route takes any loopback caller, an agent included. A hub with
// DB-backed operational settings stores the value in its profiles section,
// which outranks settings.yaml, and swaps the co-located broker's runtime
// live; a swap to the runtime already in use is a no-op.
func (c *Client) SetRuntime(ctx context.Context, rt string) error {
	if rt == "" {
		return errors.New("hubapi: setting the runtime needs a value")
	}
	if cur, err := c.Runtime(ctx); err == nil && cur == rt {
		return nil
	}
	var out systemRuntime
	applied := false
	err := retryUnanswered(ctx, func() error {
		return c.putSetting(ctx, systemRuntimePath, struct {
			Runtime string `json:"runtime"`
		}{rt}, &out)
	}, func() bool {
		cur, err := c.Runtime(ctx)
		applied = err == nil && cur == rt
		return applied
	})
	if applied {
		return nil
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

// unansweredAttempts bounds the sends of an idempotent settings PUT that got
// no answer; unansweredDelay is the pause before each resend.
const unansweredAttempts = 3

var unansweredDelay = 3 * time.Second

// putSetting sends one settings PUT, retrying a 409 (a concurrent write to
// the same settings row).
func (c *Client) putSetting(ctx context.Context, path string, in, out any) error {
	var err error
	for attempt := 0; attempt < registryConflictRetries; attempt++ {
		err = c.send(ctx, http.MethodPut, path, in, out)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
			break
		}
	}
	return err
}

// retryUnanswered runs put again when it got no answer from the hub (a
// transport error, such as curl's timeout), up to unansweredAttempts in
// all. Any answer, refusal included, ends it. landed, when not nil, reads
// the hub after an unanswered send: true means the write is there and put
// is not sent again.
func retryUnanswered(ctx context.Context, put func() error, landed func() bool) error {
	var err error
	for attempt := 0; attempt < unansweredAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(unansweredDelay):
			}
		}
		err = put()
		var ae *APIError
		if err == nil || errors.As(err, &ae) || errors.Is(err, ErrAssertionPathRefused) || errors.Is(err, errNoTransport) || ctx.Err() != nil {
			return err
		}
		if landed != nil && landed() {
			return nil
		}
	}
	return fmt.Errorf("%w (no answer after %d tries)", err, unansweredAttempts)
}
