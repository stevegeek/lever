package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const bindChildEnv = "LEVER_FSUTIL_BIND_CHILD"

// A bind mount gives one directory a second path. Under unshare -rm the test
// binary re-runs itself with base/tree bound at base/alias, base/tree/sub
// bound at base/outside and base/other/sub bound at base/elsewhere.
func TestInsideByIdentitySeesThroughABindMount(t *testing.T) {
	if base := os.Getenv(bindChildEnv); base != "" {
		tree := filepath.Join(base, "tree")
		alias := filepath.Join(base, "alias")
		// The tree named through the mount, as outer and as inner's ancestor.
		wantInside(t, filepath.Join(tree, ".lever-state"), alias, true)
		wantInside(t, filepath.Join(alias, ".lever-state"), tree, true)
		wantInside(t, filepath.Join(alias, "missing"), tree, true)
		// ".." at a mount root is the mount point's parent, so only
		// mountinfo shows that a directory of the tree is mounted outside it.
		wantInside(t, filepath.Join(base, "outside"), tree, true)
		wantInside(t, filepath.Join(base, "outside", ".lever-state"), tree, true)
		wantInside(t, filepath.Join(base, "outside", "missing", "deeper"), tree, true)
		// A subdirectory mount of something outside the tree stays outside.
		wantInside(t, filepath.Join(base, "elsewhere", ".lever-state"), tree, false)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	if out, err := exec.Command("unshare", "-rm", "true").CombinedOutput(); err != nil {
		t.Skipf("unshare -rm is not available: %v: %s", err, out)
	}
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "tree", ".lever-state"), filepath.Join(base, "tree", "sub", ".lever-state"),
		filepath.Join(base, "alias"), filepath.Join(base, "outside"), filepath.Join(base, "other", "sub", ".lever-state"),
		filepath.Join(base, "elsewhere"))
	script := `mount --bind "$1/tree" "$1/alias" && mount --bind "$1/tree/sub" "$1/outside" && ` +
		`mount --bind "$1/other/sub" "$1/elsewhere" && exec "$2" -test.run '^TestInsideByIdentitySeesThroughABindMount$' -test.v`
	cmd := exec.Command("unshare", "-rm", "sh", "-c", script, "sh", base, os.Args[0])
	cmd.Env = append(os.Environ(), bindChildEnv+"="+base)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	t.Logf("child:\n%s", out)
}

func TestUnescapeMountinfo(t *testing.T) {
	for in, want := range map[string]string{
		`/plain`:              "/plain",
		`/a\040b`:             "/a b",
		`/tab\011nl\012`:      "/tab\tnl\n",
		`/back\134slash`:      `/back\slash`,
		`/home/x//deleted`:    "/home/x//deleted",
		`/trailing\04`:        "",
		`/not-octal\09x`:      "",
		`/too-big\777rest`:    "",
		`/ends-in-backslash\`: "",
	} {
		got, err := unescapeMountinfo(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q: no error, got %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}
