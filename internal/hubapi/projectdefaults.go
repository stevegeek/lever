package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

const projectDefaultsPath = "/api/v1/admin/project-defaults"

// SetDefaultScratchpadOff turns off the hub's project default scratchpad
// (PUT /api/v1/admin/project-defaults, hub admin only). While it is on,
// scion e8160a10 and later stamp a read-write scratchpad shared dir on every
// new project and refuse to remove it from any project, so lever's strip of
// that cross-agent mount (StripSharedDir) fails. An older scion has no such
// route (404) and lets the strip through, so that is not an error. The hub
// answers with the applied value, which must read false.
func (c *Client) SetDefaultScratchpadOff(ctx context.Context) error {
	var out struct {
		DefaultScratchpad *bool `json:"default_scratchpad"`
	}
	err := retryUnanswered(ctx, func() error {
		return c.putSetting(ctx, projectDefaultsPath, struct {
			DefaultScratchpad bool `json:"default_scratchpad"`
		}{false}, &out)
	}, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if out.DefaultScratchpad == nil || *out.DefaultScratchpad {
		return &APIError{Msg: fmt.Sprintf("PUT %s: the hub does not report default_scratchpad off after the write", projectDefaultsPath)}
	}
	return nil
}
