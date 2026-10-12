//go:build linux

package voice

import "syscall"

// childSysProcAttr asks the kernel to kill the child when the tool dies,
// even by SIGKILL (the broker sends one to a tool that is slow to exit): no
// whisper-server outlives its tool holding the whisper port. The child also
// leads its own process group, which a stop signals as a whole.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}
