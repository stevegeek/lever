//go:build linux

package voice

import "syscall"

// childSysProcAttr asks the kernel to kill the child when the proxy dies,
// even by SIGKILL (`lever stop` sends one to a proxy that is slow to
// exit): no whisper-server outlives its proxy holding the voice port.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
