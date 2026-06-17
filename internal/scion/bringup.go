package scion

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
)

// hubReadyAttempts/hubReadyInterval are package vars so tests can shrink them.
var hubReadyAttempts = 30
var hubReadyInterval = 1 * time.Second

// waitHubReady polls a lightweight, PROJECT-INDEPENDENT hub call until it
// succeeds or attempts run out. `list --all` lists agents across all projects
// and hits the hub without resolving a current project — unlike `list --global`,
// which forces project resolution and fails with "no git origin remote found"
// when run (as here) before any project is registered (verified live 2026-06-17).
func (c *Client) waitHubReady(ctx context.Context) error {
	var lastErr error
	for i := 0; i < hubReadyAttempts; i++ {
		if _, err := c.run(ctx, "", "list", "--all", "--format", "json"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hubReadyInterval):
		}
	}
	return fmt.Errorf("hub not ready after %d attempts: %w", hubReadyAttempts, lastErr)
}

// InitMachine seeds the machine-level scion dir + default harness configs
// (claude/gemini). Required before `--harness claude` resolves.
func (c *Client) InitMachine(ctx context.Context) error {
	_, err := c.run(ctx, "", "init", "--machine", "--non-interactive")
	return err
}

// ConfigSetGlobal sets a global scion config key (e.g. image_registry=scionlocal).
func (c *Client) ConfigSetGlobal(ctx context.Context, key, value string) error {
	_, err := c.run(ctx, "", "config", "set", "--global", key, value)
	return err
}

// ServerStart starts the workstation daemon (Hub API + broker); it daemonises and
// returns. Dev auth is default-on, so no `hub enable` is needed.
func (c *Client) ServerStart(ctx context.Context) error {
	if _, err := c.run(ctx, "", "server", "start"); err != nil {
		return err
	}
	return c.waitHubReady(ctx)
}

// SecretSet stores a Hub secret. scion (>= da49e14) requires the value to be
// base64-encoded on input ("value must be base64-encoded", HTTP 400 otherwise)
// and decodes it for projection to the agent. We pass the raw secret and encode
// here. (Earlier vendored scion took the raw value verbatim — encoding is
// version-specific; verified against da49e14 on 2026-06-17.)
func (c *Client) SecretSet(ctx context.Context, key, value string) error {
	enc := base64.StdEncoding.EncodeToString([]byte(value))
	_, err := c.run(ctx, "", "hub", "secret", "set", key, enc)
	return err
}
