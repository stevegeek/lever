package state

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// sZomb is p_stat for a zombie (SZOMB in <sys/proc.h>).
const sZomb = 5

// processArgs is pid's argv, from the kern.procargs2 sysctl: exact
// arguments, where ps(1) would join them with spaces. The sysctl fails for
// a zombie: kern.proc.pid tells that case (errNotAProcess) from a live
// process whose argv cannot be read (Identify says Unknown).
func processArgs(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if k, kerr := unix.SysctlKinfoProc("kern.proc.pid", pid); kerr == nil && (k.Proc.P_pid != int32(pid) || k.Proc.P_stat == sZomb) {
			return nil, errNotAProcess
		}
		return nil, fmt.Errorf("%w: %v", errNoArgs, err)
	}
	return parseProcArgs2(b)
}
