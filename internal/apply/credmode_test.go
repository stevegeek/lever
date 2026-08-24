package apply

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// The manager's OAuth token is the longest-lived, highest-value secret lever
// handles, and every other credential (api_key_file, the controller PAT, the
// staged bootstrap) must be exactly 0600. `lever doctor` already fails a
// group-readable one; apply must not accept what doctor calls broken.
func TestDefaultReadCredRejectsGroupAndWorldAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o660, 0o604, 0o644, 0o666, 0o601} {
		p := filepath.Join(t.TempDir(), "oauth-token")
		if err := os.WriteFile(p, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, err := defaultReadCred(p)
		if err == nil {
			t.Fatalf("mode %#o must be rejected", mode)
		}
		testutil.WantErrContaining(t, err, "0600")
	}
}

func TestDefaultReadCredAcceptsPrivateModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		p := filepath.Join(t.TempDir(), "oauth-token")
		if err := os.WriteFile(p, []byte(" secret \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		got, err := defaultReadCred(p)
		if err != nil {
			t.Fatalf("mode %#o must be accepted: %v", mode, err)
		}
		if got != "secret" {
			t.Errorf("mode %#o: got %q, want the trimmed credential", mode, got)
		}
	}
}
