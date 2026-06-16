package scion

import "context"

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
	_, err := c.run(ctx, "", "server", "start")
	return err
}

// SecretSet stores a Hub secret. The value is the RAW secret — the vendored scion
// stores/projects it verbatim; do NOT base64-encode (see the Part-B spike).
func (c *Client) SecretSet(ctx context.Context, key, value string) error {
	_, err := c.run(ctx, "", "hub", "secret", "set", key, value)
	return err
}
