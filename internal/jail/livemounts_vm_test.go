//go:build integration

package jail

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// TestContainerLiveMountsInRealGuest runs the read-only guard's live check
// (ContainerLiveMounts: the lsns namespace check, the kernel's mount table
// and the device and inode match) through a real jail runner against real
// rootless keep-id containers in a lever-provisioned guest. Unit tests stub
// every guest answer; this is where the guest's own lsns, mountinfo and the
// tree mount's inode numbers meet the parser.
//
// Gated behind the integration tag and skipped unless LEVER_GUARD_VM_PREFIX
// is set. tools/test/lima-e2e.sh runs it against its VM; by hand:
//
//	LEVER_GUARD_VM_PREFIX='limactl,shell,<vm>' \
//	LEVER_GUARD_TREE_HOST=<host tree dir> LEVER_GUARD_TREE_GUEST=/lever \
//	  go test -tags integration ./internal/jail/ -run TestContainerLiveMountsInRealGuest -v
//
// It creates a scratch directory in the tree, an image built from the
// guest's own sleep and unshare (nothing is pulled) and two containers, and
// removes all of them.
func TestContainerLiveMountsInRealGuest(t *testing.T) {
	prefix := os.Getenv("LEVER_GUARD_VM_PREFIX")
	if prefix == "" {
		t.Skip("set LEVER_GUARD_VM_PREFIX (e.g. 'limactl,shell,<vm>'), LEVER_GUARD_TREE_HOST and LEVER_GUARD_TREE_GUEST")
	}
	hostTree, guestTree := os.Getenv("LEVER_GUARD_TREE_HOST"), os.Getenv("LEVER_GUARD_TREE_GUEST")
	if hostTree == "" || !safeGuestPath.MatchString(guestTree) {
		t.Fatal("LEVER_GUARD_TREE_HOST must be set, and LEVER_GUARD_TREE_GUEST an absolute path of [A-Za-z0-9/._-]")
	}
	pre := strings.Split(prefix, ",")
	ctx := context.Background()

	// guest runs a script in the guest as the run user, over stdin: a
	// transport that re-joins the argv leaves stdin alone. Every path put
	// into a script matches safeGuestPath, so none needs quoting.
	guest := func(script string) (proc.Result, error) {
		argv := append(append([]string{}, pre...), "sh", "-s")
		return proc.RealRunner{}.RunStdin(context.Background(), strings.NewReader("set -eu\nexport XDG_RUNTIME_DIR=/run/user/$(id -u)\n"+script), nil, argv[0], argv[1:]...)
	}
	sh := func(script string) string {
		t.Helper()
		res, err := guest(script)
		if err != nil {
			t.Fatalf("guest script failed: %v\nstdout: %s\nstderr: %s\nscript:\n%s", err, res.Stdout, res.Stderr, script)
		}
		return strings.TrimSpace(res.Stdout)
	}
	uid := sh("id -u")
	jr := New(Config{Host: proc.RealRunner{}, Prefix: pre, UID: uid})

	hostJP, err := os.MkdirTemp(hostTree, "guard-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(hostJP)
	jp := path.Join(guestTree, name)
	image, held, nested := "localhost/lever-"+name, "lever-"+name+"-held", "lever-"+name+"-nested"
	if !safeGuestPath.MatchString(jp) {
		t.Fatalf("scratch path %q", jp)
	}
	t.Cleanup(func() {
		if res, err := guest("podman rm -f -t 0 " + held + " " + nested + " >/dev/null\npodman rmi -f " + image + " >/dev/null\n"); err != nil {
			t.Logf("cleanup in the guest: %v: %s", err, res.Stderr)
		}
		if err := os.RemoveAll(hostJP); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	for _, d := range []string{"parent/entry", "tools", "kb"} {
		if err := os.MkdirAll(filepath.Join(hostJP, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fstype := sh("findmnt -n -o FSTYPE -T " + guestTree)
	t.Logf("guest: %s", sh("uname -r; lsns --version; podman --version; findmnt -n -o FSTYPE,OPTIONS -T "+guestTree))

	// A minimal image from the guest's own binaries, so the check needs no
	// registry: sleep is the first process, unshare stages the nested one.
	sh(`t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
for b in /usr/bin/sleep /usr/bin/unshare; do
  for f in "$b" $(ldd "$b" | grep -o '/[^ ]*'); do
    mkdir -p "$t$(dirname "$f")"; cp -L "$f" "$t$f"
  done
done
tar -C "$t" -cf - . | podman import - ` + image + ` >/dev/null`)

	// The mounts the manager gets for the read_only entries parent/entry,
	// tools and kb: the workspace from the tree, each parent pinned
	// read-write over itself, each entry read-only over itself. The keep-id
	// mapping is scion's.
	run := "podman run -d --pull=never --network=none " + ScionKeepIDUserns +
		" -v " + jp + ":/workspace" +
		" -v " + jp + "/parent:/workspace/parent" +
		" -v " + jp + "/parent/entry:/workspace/parent/entry:ro" +
		" -v " + jp + "/tools:/workspace/tools:ro" +
		" -v " + jp + "/kb:/workspace/kb:ro"
	sh(run + " --name " + held + " " + image + " /usr/bin/sleep infinity >/dev/null")
	// The first process itself in a user and mount namespace made inside
	// the container: what a root agent that ptraces it and calls setns
	// would leave behind.
	sh(run + " --name " + nested + " " + image + " /usr/bin/unshare --user --map-root-user --mount /usr/bin/sleep infinity >/dev/null")

	pidOf := func(ref string) string {
		t.Helper()
		for range 50 {
			if p := sh("podman inspect --type container --format '{{.State.Pid}}' " + ref); p != "0" {
				return p
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("%s did not start", ref)
		return ""
	}
	heldPID, nestedPID := pidOf(held), pidOf(nested)
	root := "/proc/" + heldPID + "/root"

	// Under rootless keep-id the pid, mnt and user namespaces of the first
	// process are as checkRuntimeNamespaces requires, and the real lsns
	// output parses.
	nsOut := sh("lsns --raw --noheadings --output NS,TYPE,ONS --task " + heldPID)
	t.Logf("lsns (held container, pid %s):\n%s", heldPID, nsOut)
	ns, err := parseNamespaces(nsOut)
	if err != nil {
		t.Fatalf("parseNamespaces(real lsns output): %v", err)
	}
	if o := ns["pid"].owner; o == 0 || ns["mnt"].owner != o || ns["user"].id != o {
		t.Fatalf("held container's namespaces = %+v: the mnt namespace's owner and the user namespace must be the pid namespace's owner", ns)
	}
	if own := sh("lsns --raw --noheadings --output NS --type user --task $$"); own == "" || own == strconv.FormatUint(ns["user"].id, 10) {
		t.Fatalf("held container in the guest shell's user namespace %q", own)
	}
	nestedOut := sh("lsns --raw --noheadings --output NS,TYPE,ONS --task " + nestedPID)
	t.Logf("lsns (nested container, pid %s):\n%s", nestedPID, nestedOut)

	want := []LiveMount{
		{Target: "/workspace/kb", Source: jp + "/kb", ReadOnly: true, SelfMount: true},
		{Target: "/workspace/parent", Source: jp + "/parent", SelfMount: true},
		{Target: "/workspace/parent/entry", Source: jp + "/parent/entry", ReadOnly: true, SelfMount: true},
		{Target: "/workspace/tools", Source: jp + "/tools", ReadOnly: true, SelfMount: true},
	}
	live := func(stage string) []LiveMountProblem {
		t.Helper()
		t.Logf("%s: mountinfo /workspace lines:\n%s", stage, sh("grep ' /workspace' /proc/"+heldPID+"/mountinfo || true"))
		t.Logf("%s: stat dev:ino (guest path, then container path):\n%s", stage, sh(
			"for d in kb parent parent/entry tools; do stat -c '%d:%i %n' "+jp+"/$d "+root+"/workspace/$d || true; done"))
		got, err := ContainerLiveMounts(ctx, jr, held, want)
		if err != nil {
			t.Fatalf("%s: ContainerLiveMounts: %v", stage, err)
		}
		t.Logf("%s: problems = %+v", stage, got)
		return got
	}

	// The real mounts pass, also after the guest drops its caches (the
	// tree mount's inode numbers must hold).
	if got := live("held"); got != nil {
		t.Fatalf("held: problems = %+v, want none", got)
	}
	sh("sync; sudo -n sh -c 'echo 3 > /proc/sys/vm/drop_caches'")
	if got := live("after drop_caches"); got != nil {
		t.Fatalf("after drop_caches: problems = %+v, want none", got)
	}

	// The nested first process is refused before any mount is read.
	if _, err := ContainerLiveMounts(ctx, jr, nested, []LiveMount{{Target: "/workspace", Source: jp}}); !errors.Is(err, ErrForeignNamespace) {
		t.Fatalf("nested container: err = %v, want ErrForeignNamespace", err)
	}
	t.Logf("nested container: ErrForeignNamespace")

	// Directories replaced on the host. Where a mount stays on the
	// directory it resolved, the check must report affected targets and no
	// other. A tree mount that reaches a directory by its path (QEMU's
	// 9p) moves the mount to the new directory instead: then nothing may
	// be reported, the container must read the new content at each target,
	// and a read-only target must still refuse a write. A mount on the old
	// directory, unreported, fails the test.
	reported := map[string]bool{}
	hostReplace := func(stage string, affected, readOnly []string, replace func() error) {
		t.Helper()
		if err := replace(); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		got := live(stage)
		var fresh []string
		for _, p := range got {
			if !reported[p.Target] {
				fresh = append(fresh, p.Target)
			}
		}
		if len(fresh) > 0 {
			// Any one problem holds the tools back, so a subset of the
			// affected targets is enough; anything else is a false alarm.
			for _, p := range fresh {
				if !slices.Contains(affected, p) {
					t.Fatalf("%s: newly reported %v, want only some of %v", stage, fresh, affected)
				}
			}
			for _, p := range fresh {
				reported[p] = true
			}
			t.Logf("%s: reported (tree mount %s)", stage, fstype)
			return
		}
		for _, target := range affected {
			if seen := sh("cat " + root + target + "/marker"); seen != "new" {
				t.Fatalf("%s: nothing reported but %s reads %q: the mount stayed on the old directory", stage, target, seen)
			}
		}
		for _, target := range readOnly {
			if res, err := guest("touch " + root + target + "/written"); err == nil || !strings.Contains(res.Stderr, "Read-only file system") {
				t.Fatalf("%s: %s accepted a write or failed otherwise (%v: %s)", stage, target, err, res.Stderr)
			}
		}
		t.Logf("%s: followed by the mount (tree mount %s): new content read, read-only kept", stage, fstype)
	}
	marker := func(rel, text string) error {
		return os.WriteFile(filepath.Join(hostJP, rel, "marker"), []byte(text+"\n"), 0o644)
	}
	for _, rel := range []string{"tools", "parent", "parent/entry"} {
		if err := marker(rel, "old"); err != nil {
			t.Fatal(err)
		}
	}
	// A rename-based deploy of an entry.
	hostReplace("tools renamed away and recreated on the host", []string{"/workspace/tools"}, []string{"/workspace/tools"}, func() error {
		if err := os.Rename(filepath.Join(hostJP, "tools"), filepath.Join(hostJP, "tools.old")); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(hostJP, "tools"), 0o755); err != nil {
			return err
		}
		return marker("tools", "new")
	})
	// An entry's parent (a pin) removed and recreated with the entry.
	hostReplace("parent removed and recreated on the host", []string{"/workspace/parent", "/workspace/parent/entry"}, []string{"/workspace/parent/entry"}, func() error {
		if err := os.RemoveAll(filepath.Join(hostJP, "parent")); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(hostJP, "parent", "entry"), 0o755); err != nil {
			return err
		}
		if err := marker("parent", "new"); err != nil {
			return err
		}
		return marker("parent/entry", "new")
	})

	// kb moved away in the guest, whose mount namespace holds no mount
	// there (the rename is refused only inside the container): the
	// container's mount moves with the directory, so the target is no
	// longer a mount point, or is one from another place.
	sh("mv " + jp + "/kb " + jp + "/kb.moved && mkdir " + jp + "/kb")
	got := live("kb moved in the guest")
	var kb []LiveMountProblem
	for _, p := range got {
		if !reported[p.Target] {
			kb = append(kb, p)
		}
	}
	if len(kb) != 1 || kb[0].Target != "/workspace/kb" || (kb[0].Reason != LiveMountMissing && kb[0].Reason != LiveMountElsewhere) {
		t.Fatalf("kb moved in the guest: newly reported %+v, want /workspace/kb missing or from elsewhere", kb)
	}
}

// safeGuestPath is a guest path the test may put into a shell script as is.
var safeGuestPath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)
