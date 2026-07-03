package brokerctl

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// BrokerPIDStatus reads the recorded broker pid and reports whether that
// process is currently alive. It is the read-only counterpart to StopBroker,
// used by `lever doctor` to tell "broker never started" from "broker died".
//
//   - found=false: no broker.pid file (broker never started, or cleanly stopped).
//   - found=true, alive=false: a stale pid file — the process is gone (or the
//     file is garbage). This is the "broker died out from under apply" case.
//   - found=true, alive=true: the recorded process is running (pid is returned).
//
// Liveness is a signal-0 probe (same technique as StopBroker); the pid could in
// principle have been recycled by the OS, but on the single-operator workstation
// this targets that window is small.
func (s State) BrokerPIDStatus() (pid int, found, alive bool) {
	data, err := os.ReadFile(s.PID())
	if err != nil {
		return 0, false, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, true, false // garbage pid file: found, but not a live process
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, true, false
	}
	return pid, true, proc.Signal(syscall.Signal(0)) == nil
}
