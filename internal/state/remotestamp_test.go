package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// runningProxyPID records pid in remote.pid, as a bound proxy would. A stamp
// is a statement about a running process, so no test can write one without
// it.
func runningProxyPID(t *testing.T, s State, pid int) {
	t.Helper()
	if err := os.WriteFile(s.RemotePID(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRemoteStampRoundTripAndMismatch: a stamp matches only the exact
// version and config it recorded, and every doubt reads as a mismatch.
func TestRemoteStampRoundTripAndMismatch(t *testing.T) {
	s := ForConfig(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runningProxyPID(t, s, 4242)

	// Absent file: mismatch, never trusted.
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("an absent stamp must not match")
	}

	if err := s.WriteRemoteStamp("v1", "h1"); err != nil {
		t.Fatal(err)
	}
	if !s.RemoteStampMatches("v1", "h1") {
		t.Error("a stamp must match what it recorded")
	}
	if s.RemoteStampMatches("v2", "h1") {
		t.Error("a different BINARY must not match — a new lever may serve differently")
	}
	if s.RemoteStampMatches("v1", "h2") {
		t.Error("a different CONFIG must not match — this is the allowed_users defect")
	}

	if err := os.WriteFile(s.RemoteStamp(), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("a truncated stamp must not match")
	}

	// Owner-only, like the rest of the state dir.
	if err := s.WriteRemoteStamp("v1", "h1"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.RemoteStamp())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode = %v, want 0600", fi.Mode().Perm())
	}
	if filepath.Dir(s.RemoteStamp()) != s.Dir {
		t.Error("the stamp must live beside remote.pid in the state dir")
	}
}

// TestRemoteStampIsBoundToTheProxyThatOwnsThePIDFile: a proxy started by
// hand that takes over remote.pid without stamping must not inherit a stamp
// another process left, or apply would vouch for a process enforcing a
// config it never read.
func TestRemoteStampIsBoundToTheProxyThatOwnsThePIDFile(t *testing.T) {
	s := ForConfig(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}

	runningProxyPID(t, s, 4242)
	if err := s.WriteRemoteStamp("v1", "h1"); err != nil {
		t.Fatal(err)
	}
	if !s.RemoteStampMatches("v1", "h1") {
		t.Fatal("a stamp must match while the proxy it describes still owns remote.pid")
	}

	// Another proxy takes over remote.pid without stamping: the inherited
	// stamp must not vouch for it.
	runningProxyPID(t, s, 4343)
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("a stamp must not describe a proxy that did not write it")
	}

	// That proxy stamping its own config makes the stamp usable again, and
	// the other config now fails to match.
	if err := s.WriteRemoteStamp("v1", "h2"); err != nil {
		t.Fatal(err)
	}
	if !s.RemoteStampMatches("v1", "h2") {
		t.Error("the running proxy's own stamp must match its own config")
	}
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("the replaced proxy's config must no longer match")
	}

	// No proxy at all: a stamp left behind after the process exits (Serve
	// removes remote.pid on shutdown) vouches for nothing.
	if err := os.Remove(s.RemotePID()); err != nil {
		t.Fatal(err)
	}
	if s.RemoteStampMatches("v1", "h2") {
		t.Error("a stamp with no remote.pid beside it must not match")
	}
}

// TestWriteRemoteStampLeavesNoStaleRecordOnFailure: callers keep serving
// after a stamp write failure, which is only safe because a failure leaves
// no stamp.
func TestWriteRemoteStampLeavesNoStaleRecordOnFailure(t *testing.T) {
	s := ForConfig(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runningProxyPID(t, s, 4242)
	if err := s.WriteRemoteStamp("v1", "h1"); err != nil {
		t.Fatal(err)
	}

	// A garbage pid file is the general "cannot tell what is running" case.
	if err := os.WriteFile(s.RemotePID(), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRemoteStamp("v1", "h1"); err == nil {
		t.Error("writing a stamp with no readable proxy pid must fail")
	}
	if _, err := os.Stat(s.RemoteStamp()); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a failed write must remove the previous stamp, not leave it describing a process nothing can identify")
	}
}
