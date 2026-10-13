//go:build linux || darwin

package treebackup

import "golang.org/x/sys/unix"

func retryEINTR[T any](f func() (T, error)) (T, error) {
	for {
		v, err := f()
		if err != unix.EINTR {
			return v, err
		}
	}
}
