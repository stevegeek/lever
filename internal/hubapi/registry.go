package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

const systemRegistryPath = "/api/v1/system/registry"

// SetImageRegistry sets the hub's agent image registry prefix
// (PUT /api/v1/system/registry) and requires the hub to echo it back.
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
	if err := c.send(ctx, http.MethodPut, systemRegistryPath, struct {
		ImageRegistry string `json:"image_registry"`
	}{registry}, &out); err != nil {
		return err
	}
	if out.ImageRegistry == nil || *out.ImageRegistry != registry {
		return &APIError{Msg: fmt.Sprintf("PUT %s: the hub did not echo image_registry %q; the hub API changed shape", systemRegistryPath, registry)}
	}
	return nil
}
