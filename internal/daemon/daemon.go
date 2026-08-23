// Package daemon holds the pid-file and listener bookkeeping shared by the
// host-side daemons (`lever broker serve`, `lever remote serve`).
package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/state"
)

// WritePIDFile records the running process's pid at path (0600), creating
// the parent directory if needed. Callers write it only after their listeners
// have bound — so a pid file on disk means a daemon is (or was) actually
// serving, never a failed-bind ghost. A no-op when path is empty (tests that
// don't care about pid tracking). Returns an error: a pid file we cannot
// write is a doctor blind spot, so callers treat it as fatal.
func WritePIDFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("daemon: pid dir: %w", err)
	}
	if err := state.WriteFileAtomic(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("daemon: write pid: %w", err)
	}
	return nil
}

// RemovePIDFile deletes the pid file on shutdown. A removal failure is a
// warning (printed to stderr), not fatal (the process is exiting anyway); an
// already-absent file is fine. A no-op when path is empty.
func RemovePIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		Warnf("could not remove %s: %v", path, err)
	}
}

// Warnf prints a non-fatal problem to stderr in the "lever: warning:" form
// every host-side daemon uses.
func Warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lever: warning: "+format+"\n", args...)
}

// CloseListeners closes every non-nil listener, discarding errors: a
// double-close (a server may already have closed them on a fail-closed path)
// surfaces as ErrClosed, which is benign here.
func CloseListeners(lns ...net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			_ = ln.Close()
		}
	}
}
