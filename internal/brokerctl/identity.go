package brokerctl

import (
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// ConfigHash digests the broker-relevant configuration: the broker block
// (tools, ports, knobs) and the workers list (worker specs feed the broker's
// dispatch table). apply's broker-reuse shortcut compares a running broker's
// reported hash against this to decide whether the broker must be restarted
// on re-apply (#19) — so the hash must cover exactly the config a running
// broker bakes in at start, and nothing else (a manager-image change must
// NOT bounce the broker).
// It also covers the scion block, so a declared scion change bounces the
// broker. Note what that does NOT buy: `scion.source` and `scion.binary` are
// PATHS, so replacing the artifact behind one leaves this hash byte-identical
// and a running broker survives it. Nothing here can see that. It is why the
// `--role` capability probe no longer memoises its answer (see
// scion.Client.roleFlagSupported) instead of trusting a restart to invalidate
// it — a stale "no --role" would hand every agent scion#1090's FULL default.
func ConfigHash(app *config.App) string {
	// Marshal of plain config structs cannot fail in practice; an empty hash
	// (HashJSON's failure value) makes the comparison a guaranteed mismatch
	// (restart), which fails toward the safe side.
	return state.HashJSON(struct {
		Broker  config.Broker
		Workers []config.Worker
		Scion   config.ScionConfig
	}{app.Broker, app.Workers, app.Scion})
}

// RemoteConfigHash digests the config a `lever remote serve` process captures
// at startup — see state.RemoteConfigHash for why. This is the only place
// that maps config.App onto state.RemoteIdentity, so state stays free of
// config types.
func RemoteConfigHash(app *config.App) string {
	return state.RemoteConfigHash(state.RemoteIdentity{
		Enabled:      app.Remote.Enabled,
		Port:         app.Remote.Port,
		BaseURL:      app.Remote.BaseURL,
		AllowedUsers: app.Remote.AllowedUsers,
		LoginPort:    app.Remote.LoginPort,
		Name:         app.Name,
		Backend:      app.Backend,
	})
}
