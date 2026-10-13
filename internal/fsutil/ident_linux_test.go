package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const bindChildEnv = "LEVER_FSUTIL_BIND_CHILD"

// A bind mount gives one directory a second path. Under unshare -rm the test
// binary re-runs itself with base/tree bound at base/alias and base/tree/sub
// bound at base/outside.
func TestInsideByIdentitySeesThroughABindMount(t *testing.T) {
	if base := os.Getenv(bindChildEnv); base != "" {
		tree := filepath.Join(base, "tree")
		alias := filepath.Join(base, "alias")
		// The tree named through the mount, as outer and as inner's ancestor.
		wantInside(t, filepath.Join(tree, ".lever-state"), alias, true)
		wantInside(t, filepath.Join(alias, ".lever-state"), tree, true)
		wantInside(t, filepath.Join(alias, "missing"), tree, true)
		// The stated limit: ".." at a mount root is the mount point's parent,
		// so a directory of the tree mounted outside it is not seen.
		wantInside(t, filepath.Join(base, "outside"), tree, false)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	if out, err := exec.Command("unshare", "-rm", "true").CombinedOutput(); err != nil {
		t.Skipf("unshare -rm is not available: %v: %s", err, out)
	}
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "tree", ".lever-state"), filepath.Join(base, "tree", "sub"),
		filepath.Join(base, "alias"), filepath.Join(base, "outside"))
	script := `mount --bind "$1/tree" "$1/alias" && mount --bind "$1/tree/sub" "$1/outside" && exec "$2" -test.run '^TestInsideByIdentitySeesThroughABindMount$' -test.v`
	cmd := exec.Command("unshare", "-rm", "sh", "-c", script, "sh", base, os.Args[0])
	cmd.Env = append(os.Environ(), bindChildEnv+"="+base)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	t.Logf("child:\n%s", out)
}
