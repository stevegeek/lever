package webassets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/provision/scionbin"
)

// pinsDir, under CacheRoot, maps an immutable scion pin to the digest of the
// source build made from it: one file per pin, named by the pin and holding
// the digest. Without it, finding the cached build needs the module's source
// dir, and `go mod download` of a pin given as a commit-hash prefix asks the
// module proxy every time, even with the module cached. So every lever
// command that converges the web UI (up, apply, conversation new) depended
// on reaching proxy.golang.org. The binary's build stamp (scionbin) is the
// same idea, with the same assumption: an all-hex pin is a commit, not a
// branch that happens to be spelled in hex.
const pinsDir = "pins"

var digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PinnedBuild returns the completed source build recorded for version by
// RecordPinnedBuild. It runs no command and touches no network. ok is false
// for a version that can move (a branch, "latest"), a pin with no record, a
// record that is not a digest, and a build that is no longer complete in the
// cache; the caller then fetches the sources and builds as before.
func PinnedBuild(version string) (dist, digest string, ok bool) {
	p, err := pinPath(version)
	if err != nil {
		return "", "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", false
	}
	digest = strings.TrimSpace(string(b))
	if !digestRe.MatchString(digest) {
		return "", "", false
	}
	buildDir := filepath.Join(filepath.Dir(filepath.Dir(p)), digest)
	if !webBuildComplete(buildDir) {
		return "", "", false
	}
	return filepath.Join(buildDir, "dist", "client"), digest, true
}

// RecordPinnedBuild records that version's sources build to digest, so the
// next PinnedBuild finds it offline. A version that can move is not recorded
// (nil). The write is a rename from a temporary file, so a reader sees the
// old record or the new one, never half of one.
func RecordPinnedBuild(version, digest string) error {
	if !digestRe.MatchString(digest) {
		return fmt.Errorf("record scion web build for %s: %q is not a source digest", version, digest)
	}
	p, err := pinPath(version)
	if errors.Is(err, errMutablePin) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record scion web build for %s: %w", version, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("record scion web build for %s: %w", version, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".pin-")
	if err != nil {
		return fmt.Errorf("record scion web build for %s: %w", version, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.WriteString(digest)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("record scion web build for %s: %w", version, werr)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("record scion web build for %s: %w", version, err)
	}
	return nil
}

// errMutablePin: the version can move, so no record is kept for it.
var errMutablePin = errors.New("scion version can move; no pinned web build")

// pinPath is the record's path for an immutable version. ImmutableVersion's
// pattern holds no path separator and no leading dot, so the name stays in
// pinsDir.
func pinPath(version string) (string, error) {
	if !scionbin.ImmutableVersion(version) {
		return "", errMutablePin
	}
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, pinsDir, version), nil
}
