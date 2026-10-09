//go:build !linux

package voice

import "syscall"

// childSysProcAttr is nothing off Linux: the child is stopped by Run when
// the proxy's context ends (on SIGTERM), and a port still held by an
// orphan is left alone at the next start (Supervisor.portTaken).
func childSysProcAttr() *syscall.SysProcAttr { return nil }
