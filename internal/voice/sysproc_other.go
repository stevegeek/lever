//go:build !linux

package voice

import "syscall"

// childSysProcAttr is nothing off Linux: the child is stopped by Run when
// the tool's context ends (on SIGTERM, which the broker sends before its
// SIGKILL), and a port still held by an orphan (a tool killed outright) is
// left alone at the next start (Supervisor.portTaken).
func childSysProcAttr() *syscall.SysProcAttr { return nil }
