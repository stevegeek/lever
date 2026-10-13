package host

import "errors"

// Sentinel errors the host verbs wrap with %w so callers match with errors.Is,
// not wording. Each sentinel's text is part of the user-facing message.
var (
	// errJailNotUp is wrapped by the passive verbs (attach, msg) when the
	// jail's run user cannot be resolved; the operator must run `lever up`.
	errJailNotUp = errors.New("jail not up")

	errRemoteDisabled = errors.New("remote access is disabled — set remote.enabled: true in the config")

	// errRemoteProxyNotListening: a spawned proxy never answered on its port
	// within the start budget.
	errRemoteProxyNotListening = errors.New("the remote proxy started but is not listening")
)
