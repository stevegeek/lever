//go:build !linux && !darwin

package state

// processArgs cannot read a process's arguments on this OS, so no pid
// counts as a lever daemon: nothing is signalled on a pid alone.
func processArgs(int) ([]string, error) { return nil, errNoArgs }
