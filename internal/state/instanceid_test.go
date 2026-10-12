package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestInstanceIDMadeOnceAndKept(t *testing.T) {
	s := State{Dir: filepath.Join(t.TempDir(), DirName)}
	if _, err := s.ReadInstanceID(); !errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("before first use: %v, want ErrNoInstanceID", err)
	}
	id, err := s.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidInstanceID(id) {
		t.Fatalf("id %q has the wrong shape", id)
	}
	again, err := s.EnsureInstanceID()
	if err != nil || again != id {
		t.Fatalf("second Ensure = %q, %v; want %q", again, err, id)
	}
	if got, err := s.ReadInstanceID(); err != nil || got != id {
		t.Fatalf("Read = %q, %v", got, err)
	}
	fi, err := os.Stat(s.InstanceIDFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	// No temp file is left behind.
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 1 {
		t.Fatalf("state dir holds %d entries, want only the id file", len(entries))
	}
}

// Two instances never share an id: each state dir makes its own.
func TestInstanceIDDiffersPerInstance(t *testing.T) {
	a, _ := State{Dir: t.TempDir()}.EnsureInstanceID()
	b, _ := State{Dir: t.TempDir()}.EnsureInstanceID()
	if a == "" || a == b {
		t.Fatalf("ids %q and %q", a, b)
	}
}

// A copied instance directory carries the file, but the copy makes its own
// id; the original keeps its own.
func TestInstanceIDOfACopyIsNew(t *testing.T) {
	orig := State{Dir: t.TempDir()}
	id, err := orig.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(orig.InstanceIDFile())
	if err != nil {
		t.Fatal(err)
	}
	cp := State{Dir: t.TempDir()}
	if err := os.WriteFile(cp.InstanceIDFile(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.ReadInstanceID(); !errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("copy Read: %v, want ErrNoInstanceID", err)
	}
	cid, err := cp.EnsureInstanceID()
	if err != nil || cid == id || !ValidInstanceID(cid) {
		t.Fatalf("copy id %q (%v), original %q", cid, err, id)
	}
	if got, _ := orig.ReadInstanceID(); got != id {
		t.Fatalf("original id changed to %q", got)
	}
}

// The same directory reached through a symlink keeps its id.
func TestInstanceIDThroughASymlink(t *testing.T) {
	real := t.TempDir()
	id, err := State{Dir: real}.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if got, err := (State{Dir: link}).EnsureInstanceID(); err != nil || got != id {
		t.Fatalf("via symlink = %q, %v; want %q", got, err, id)
	}
}

// A malformed file is an error, never silently replaced.
func TestInstanceIDMalformedFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"not json": "zzz",
		"bad id":   `{"id":"NOT-HEX","dir":"/x"}`,
		"rel dir":  `{"id":"0123456789abcdef0123456789abcdef","dir":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := State{Dir: t.TempDir()}
			if err := os.WriteFile(s.InstanceIDFile(), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.EnsureInstanceID(); err == nil || errors.Is(err, ErrNoInstanceID) {
				t.Fatalf("Ensure = %v, want a malformed-file error", err)
			}
			if b, _ := os.ReadFile(s.InstanceIDFile()); string(b) != body {
				t.Fatal("malformed file was rewritten")
			}
		})
	}
}

// Concurrent first uses agree on one id.
func TestInstanceIDConcurrentFirstUse(t *testing.T) {
	s := State{Dir: t.TempDir()}
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id, err := s.EnsureInstanceID()
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
}

func TestValidInstanceID(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789abcdef0123456789abcdef":  true,
		"0123456789ABCDEF0123456789abcdef":  false,
		"0123456789abcdef0123456789abcde":   false,
		"0123456789abcdef0123456789abcdef0": false,
		"":                                  false,
	} {
		if ValidInstanceID(id) != want {
			t.Errorf("ValidInstanceID(%q) != %v", id, want)
		}
	}
}
