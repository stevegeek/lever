package state

import "golang.org/x/sys/unix"

// processArgs is pid's argv, from the kern.procargs2 sysctl: exact
// arguments, where ps(1) would join them with spaces. It fails for a
// process of another user and for a zombie.
func processArgs(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcArgs2(b)
}
