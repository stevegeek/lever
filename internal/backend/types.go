package backend

import "github.com/stevegeek/lever/internal/backend/guest"

// The data types the Backend interface carries for its guest-side verbs are
// declared in guest, the leaf that fills them in and consumes them. These
// aliases let the contract name them without guest depending on the contract.
type (
	HubLogin          = guest.HubLogin
	ScionProjectState = guest.ScionProjectState
	ScionProjectEntry = guest.ScionProjectEntry
)
