//go:build !unix

package host

import "syscall"

func detachedAttr() *syscall.SysProcAttr { return nil }

func lowerPriority() {}
