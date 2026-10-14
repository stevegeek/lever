//go:build unix

package host

import "syscall"

// detachedAttr puts the background GC in its own session, so the terminal's
// Ctrl-C and hangup do not reach it.
func detachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func lowerPriority() { _ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10) }
