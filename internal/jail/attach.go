package jail

import "slices"

// AttachArgv builds the host argv to attach an interactive scion command INSIDE
// the jail. It mirrors Runner.RunIn's prefix+env shape (see runner.go) but
// returns the argv for the caller to exec() directly — interactive TTY handover
// can't go through the Runner. inner is the in-jail command (e.g. the argv from
// scion.Client.AttachArgv). The shared jail env comes from jailEnvFor.
func AttachArgv(prefix []string, uid string, inner []string) []string {
	return slices.Concat(prefix, []string{"env"}, jailEnvFor(uid), inner)
}
