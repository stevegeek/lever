package host

import "errors"

// Sentinel errors the host verbs wrap with %w so callers and tests can match
// the condition with errors.Is instead of the wording. The wording of each
// sentinel is part of the user-facing message it is wrapped into.
var (
	// errJailNotUp is wrapped by the passive verbs (attach, msg) when the
	// jail's run user cannot be resolved; the operator must run `lever up`.
	errJailNotUp = errors.New("jail not up")

	// errRemoteDisabled is returned by the remote gates when remote.enabled
	// is not set.
	errRemoteDisabled = errors.New("remote access is disabled — set remote.enabled: true in the config")

	// errRemoteProxyNotListening is wrapped when a spawned remote proxy
	// never answers on its port inside the start budget.
	errRemoteProxyNotListening = errors.New("the remote proxy started but is not listening")
)
