//go:build !linux

package voice

// listenerOwnedBy cannot tell off Linux: the caller trusts the port it
// found free before it started the child (Supervisor.portTaken).
func listenerOwnedBy(pid, port int) (owned, known bool) { return false, false }
