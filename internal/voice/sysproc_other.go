//go:build unix && !linux

package voice

import "syscall"

// childSysProcAttr starts the child in its own process group, which a stop
// signals as a whole. There is no Pdeathsig off Linux: the child is stopped
// by Run when the tool's context ends (on SIGTERM, which the broker sends
// before its SIGKILL), and one a tool killed outright left running is found
// through the pid file and stopped at the next start (Supervisor.reapOrphan).
func childSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
