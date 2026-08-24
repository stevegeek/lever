package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// runningProxyPID pretends a proxy is running by recording pid in remote.pid,
// the file remoteproxy.Serve writes for itself once it is bound. The stamp is
// written and read against that file, so no test can produce a stamp without
// one — which is the point: a stamp is a statement about a running process.
func runningProxyPID(t *testing.T, s State, pid int) {
	t.Helper()
	if err := os.WriteFile(s.RemotePID(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRemoteStampRoundTripAndMismatch pins the contract apply's reuse shortcut
// depends on: a stamp matches only the exact version+config it recorded, and
// every doubt reads as a mismatch (so the proxy restarts rather than serving a
// config the operator has since changed).
func TestRemoteStampRoundTripAndMismatch(t *testing.T) {
	s := ForConfig(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runningProxyPID(t, s, 4242)

	// Absent file: mismatch, not a crash — a proxy started by an older lever
	// left no stamp and must be restarted, never trusted.
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

	// Truncated/garbage content reads as a mismatch rather than matching by
	// accident or erroring.
	if err := os.WriteFile(s.RemoteStamp(), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("a truncated stamp must not match")
	}

	// The stamp names a proxy credential-adjacent state dir; keep it owner-only.
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

// TestRemoteStampIsBoundToTheProxyThatOwnsThePIDFile is the "stale but
// matching" defect. remote.pid is written by EVERY `lever remote serve`, while
// the stamp used to be written only by apply — so a proxy started by hand took
// over the pid file and inherited the stamp a previous apply had left. Alive,
// listening, stamp matching: apply reported success while a process it had
// never started enforced a config it had never read.
//
// remoteproxy.Serve now writes the stamp itself, and the stamp names its pid,
// so a starter that takes the pid file without stamping cannot inherit one.
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

	// Another proxy takes over remote.pid without stamping — an older lever,
	// or anything else that starts one. The inherited stamp must not vouch
	// for it.
	runningProxyPID(t, s, 4343)
	if s.RemoteStampMatches("v1", "h1") {
		t.Error("a stamp must not describe a proxy that did not write it")
	}

	// That proxy stamping its OWN config is what makes the stamp usable again
	// — and it is the other config that now fails to match.
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

// TestWriteRemoteStampLeavesNoStaleRecordOnFailure: callers treat a stamp
// write failure as a warning and keep serving (remoteproxy.Serve), which is
// only safe because a failure leaves NO stamp. An absent stamp costs the next
// apply a redundant restart; a stale one costs it the check entirely.
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
