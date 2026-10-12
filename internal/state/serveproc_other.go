//go:build !linux && !darwin

package state

// processArgs cannot read a process's arguments on this OS: every live pid
// is Unknown, so nothing is signalled on a pid alone, and a stop says so.
func processArgs(int) ([]string, error) { return nil, errNoArgs }
