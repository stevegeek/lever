package guest

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/backend/types"
)

// The guest half of lever's remote-access login path.
//
// scion's web UI is opened by a session cookie, which lever obtains by driving
// scion's OIDC login server-side from the host proxy (internal/remoteproxy).
// Two facts decide the shape of everything below:
//
//   - The hub validates the issuer URL AT STARTUP and refuses to start unless
//     it is https, or http on localhost/127.0.0.1 (pkg/hub/oauth.go
//     validateOIDCLoginConfig). So the hub can only be pointed at a provider
//     it believes is local.
//   - The provider must run host-side, because authorization codes must be
//     mintable ONLY by an in-process call inside the proxy. Splitting it —
//     HTTP surface in the guest, minting on the host — would put a minted code
//     on a channel crossing into the jail, which is the entire problem being
//     avoided.
//
// So: a logic-free forwarder listens on the guest's loopback at the provider's
// port and carries bytes to the host (internal/provision/loginfwd, which also
// BUILDS it on the host), and the hub's oidc_login block names
// http://127.0.0.1:<port> as the issuer. The hub reads that block once, at
// startup, so EnsureHubLogin reports whether it changed and the caller
// restarts the hub when it did.
// EnsureHubLogin puts the guest into the state lever's remote login needs:
// the forwarder built, installed and listening, and the hub's oidc_login block
// present in ~/.scion/settings.yaml.
//
// It reports whether the hub's configuration CHANGED. scion reads that file
// once, at startup, so a change means a running hub is still serving the old
// configuration and the caller must restart it. An unchanged config restarts
// nothing, which is what keeps a re-apply from bouncing the hub (and every
// agent's connection to it) for no reason.
func (g Guest) EnsureHubLogin(ctx context.Context, spec types.HubLogin) (bool, error) {
	if spec.IssuerPort <= 0 || spec.HostPort <= 0 || spec.HostAddress == "" || spec.ClientID == "" {
		return false, fmt.Errorf("guest: hub login: issuer port, host port, host address and client id are all required")
	}
	if spec.IssuerPort == spec.HostPort {
		// OrbStack mirrors the guest listener onto the host at the same
		// number, so one number for both halves means the provider cannot
		// bind its own port. See config.GuestLoginIssuerPort.
		return false, fmt.Errorf("guest: hub login: the guest issuer port and the host port must differ (both are %d)", spec.HostPort)
	}
	// The forwarder first: once the hub restarts with oidc_login set, its very
	// first login attempt dials the issuer, and finding nothing there caches
	// a broken discovery for the length of one request rather than working.
	if err := g.ensureLoginForwarder(ctx, spec); err != nil {
		return false, err
	}
	return g.ensureHubLoginSettings(ctx, spec)
}

// DisableHubLogin converges the guest's login path OFF: it stops the
// forwarder, removes it, and drops the `oidc_login` block from the hub's
// settings.
//
// The forwarder is the part that matters. It is an unauthenticated TCP bridge
// from guest loopback to a host loopback port, reachable from every agent's
// network namespace, and it outlives the feature it was installed for: with
// remote access turned back off, the host-side provider is gone but a port
// beside lever's own 8443/8444/8445 stays bridged into the jail, so any FUTURE
// host listener that lands there would be reachable from inside it. Stopping
// the proxy is not enough; the bridge has to go too.
//
// It reports whether it removed HUB CONFIGURATION — the `oidc_login` block, or
// the `display_name` lever wrote beside it. That is the OFF path's half of the
// signal EnsureHubLogin gives the ON path, and it is there for the same reason:
// the hub reads that file once, at startup, so a hub that is already running
// was started FROM the file this just edited and goes on advertising a login
// whose provider has gone. Only a restart replaces that, and only the caller
// can order one (internal/apply.Run).
//
// The forwarder is deliberately NOT part of that answer. No hub ever reads it,
// and a restart drops every agent's connection to the hub — so the signal has
// to mean "the hub is serving something this apply took away", not merely
// "something was removed".
//
// Every apply of every instance with remote access off pays for both halves, so
// both have to be quiet when there is nothing to do: the guest script exits
// early on a missing binary, and hubSettingsWithoutLogin reports
// (unchanged, false) for every "nothing there" shape. A converged instance
// costs two round trips, writes nothing, and reports false.
func (g Guest) DisableHubLogin(ctx context.Context) (bool, error) {
	if _, err := g.RootRun(ctx, "bash", "-c", disableLoginForwardScript); err != nil {
		return false, fmt.Errorf("guest: stop the login forwarder: %w", err)
	}
	// The settings edit is NOT gated on having found the binary. It used to be,
	// and that made the failure permanent: the script `rm -f`s the binary
	// BEFORE the settings edit runs, so if that edit ever failed (a transient
	// guest error, the machine going away mid-command), the next apply found no
	// binary, returned early, and the `oidc_login` block stayed in the guest
	// forever — advertising a login for a provider that no longer runs, with no
	// lever verb that removes it. Convergence must not depend on a step that
	// already succeeded. removeHubLoginSettings is itself a quiet no-op when
	// there is nothing to remove (see hubSettingsWithoutLogin).
	return g.removeHubLoginSettings(ctx)
}
