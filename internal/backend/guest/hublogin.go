package guest

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/backend/types"
)

// The guest half of lever's remote-access login path:
//
//   - The hub refuses to start unless the issuer URL is https, or http on
//     localhost/127.0.0.1, so it can only be pointed at a provider it
//     believes is local.
//   - The provider must run host-side: authorization codes must be mintable
//     ONLY by an in-process call inside the proxy, never on a channel that
//     crosses into the jail.
//
// So a logic-free forwarder listens on the guest's loopback and carries bytes
// to the host provider, and the hub's oidc_login block names
// http://127.0.0.1:<port> as the issuer.

// EnsureHubLogin installs and starts the forwarder and writes the hub's
// oidc_login block into ~/.scion/settings.yaml.
//
// It reports whether the hub's configuration CHANGED. scion reads that file
// only at startup, so the caller must restart a running hub on a change, and
// must not on no change.
func (g Guest) EnsureHubLogin(ctx context.Context, spec types.HubLogin) (bool, error) {
	if spec.IssuerPort <= 0 || spec.HostPort <= 0 || spec.HostAddress == "" || spec.ClientID == "" {
		return false, fmt.Errorf("guest: hub login: issuer port, host port, host address and client id are all required")
	}
	if spec.IssuerPort == spec.HostPort {
		// OrbStack mirrors the guest listener onto the host at the same
		// number, so the provider could not bind its own port.
		return false, fmt.Errorf("guest: hub login: the guest issuer port and the host port must differ (both are %d)", spec.HostPort)
	}
	// The forwarder first: the hub's first login attempt after a restart
	// dials the issuer.
	if err := g.ensureLoginForwarder(ctx, spec); err != nil {
		return false, err
	}
	return g.ensureHubLoginSettings(ctx, spec)
}

// DisableHubLogin stops and removes the forwarder and drops the `oidc_login`
// block from the hub's settings.
//
// The forwarder is an unauthenticated TCP bridge from guest loopback to a host
// loopback port, reachable from every agent's network namespace. Left behind,
// any future host listener on that port would be reachable from the jail, so
// the bridge must go, not just the proxy.
//
// It reports whether it removed HUB CONFIGURATION (the `oidc_login` block or
// the `display_name` lever wrote beside it): a running hub read the file at
// startup and goes on advertising the login until the caller restarts it. The
// forwarder is deliberately not part of that answer, because no hub reads it
// and a restart drops every agent's connection. A converged instance writes
// nothing and reports false.
func (g Guest) DisableHubLogin(ctx context.Context) (bool, error) {
	if _, err := g.RootRun(ctx, "bash", "-c", disableLoginForwardScript); err != nil {
		return false, fmt.Errorf("guest: stop the login forwarder: %w", err)
	}
	// The settings edit must NOT be gated on having found the binary: the
	// script removes the binary first, so a failed edit would otherwise never
	// be retried and the block would stay forever.
	return g.removeHubLoginSettings(ctx)
}
