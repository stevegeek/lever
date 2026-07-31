package broker

import "github.com/stevegeek/lever/internal/cap/ca"

// lapseFunc returns the natural-lapse observation hook for the mTLS listener.
// Placeholder until the auto-re-enrol healer lands (this commit series).
func (b *Broker) lapseFunc() ca.LapseFunc { return nil }
