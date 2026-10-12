//go:build !unix

package voice

import "syscall"

// childSysProcAttr is nothing off unix.
func childSysProcAttr() *syscall.SysProcAttr { return nil }
