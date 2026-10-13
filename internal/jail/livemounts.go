package jail

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// LiveMount is one directory of a container's mount plan, as
// ContainerLiveMounts checks it: Target is the in-container mount point,
// Source the guest path the mount must cover, ReadOnly whether it must be
// mounted read-only. SelfMount says the mount is a directory of the tree
// mounted over its own place: Target lies under another mount of the same
// tree (the workspace, or a pin), at the same relative path as Source, so
// the kernel's record of where the mount comes from must be that parent's
// device, and its root joined with that path (see LiveMountElsewhere).
type LiveMount struct {
	Target, Source string
	ReadOnly       bool
	SelfMount      bool
}

// LiveMountProblem is one way a running container falls short of a
// LiveMount. Reason is one of the fixed LiveMount* texts below, never
// guest output.
type LiveMountProblem struct {
	Target, Reason string
}

const (
	LiveMountMissing  = "not a mount point in the running container"
	LiveMountWritable = "mounted read-write in the running container"
	LiveMountReplaced = "covers another directory than the one on the host (it was replaced after the container started)"
	// LiveMountElsewhere: by the kernel's mount table, the mount does not
	// come from the tree directory at its place (another device, or another
	// path in the tree's file system): the runtime followed a symbolic link
	// that stood in the path when it mounted, or the directory was moved
	// away (renamed) since through the guest's own view of the tree. It needs no stable inode numbers, unlike
	// LiveMountReplaced.
	LiveMountElsewhere = "is mounted from another place than the tree's directory there (a symbolic link was followed when the container started, or the directory was moved since)"
)

// liveMountsMaxOutput bounds what ContainerLiveMounts reads from the guest.
const liveMountsMaxOutput = 64 << 10

// errLiveMounts is the one error ContainerLiveMounts returns for anything
// it cannot read or parse: fixed text, so no guest or container output
// reaches a log through it.
var errLiveMounts = errors.New("cannot read the running container's mounts from the guest")

// ErrContainerNotRunning: the container has no process to read mounts from.
var ErrContainerNotRunning = errors.New("the container is not running")

// ErrForeignNamespace: the container's first process is not in the mount
// namespace its runtime made for it, so what it sees says nothing about the
// mounts the agent works under. Fixed text, no guest output.
var ErrForeignNamespace = errors.New("the container's first process is in a mount namespace made inside the container, not the one its runtime created")

// ContainerLiveMounts checks the running container ref against want from
// the GUEST side, without running a program inside the container (the
// agent controls it, and lever treats it as root there: any of them may be
// replaced). Through r, as the
// guest user that owns the rootless container:
//
//   - podman inspect gives the container's process id;
//   - that process must still be in the namespaces its runtime created
//     (checkRuntimeNamespaces, before and after the reads below);
//   - /proc/<pid>/mountinfo, which the kernel writes, must list each
//     Target as a mount point (the topmost entry counts), read-only where
//     ReadOnly is set;
//   - stat must find the same device and inode at Source and at
//     /proc/<pid>/root<Target>: a directory replaced on the host leaves
//     the mount on the old one. The paths travel on stdin, NUL-separated,
//     never as words of the guest command line: a transport that joins
//     and re-splits the argv cannot cut a path with a space in two;
//   - the process id is read again at the end and must not have changed.
//
// Every read is bounded in time and size (BoundAgentExec), and runs under
// the guest's own deadline too (runGuestBounded), so a read that hangs
// does not stay behind in the guest. Output is parsed defensively;
// anything unexpected is an error, never "held".
func ContainerLiveMounts(ctx context.Context, r proc.Runner, ref string, want []LiveMount) ([]LiveMountProblem, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return nil, fmt.Errorf("checking container mounts: invalid container reference %q", ref)
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("checking container mounts: nothing to check")
	}
	for _, w := range want {
		if !strings.HasPrefix(w.Target, "/") || !strings.HasPrefix(w.Source, "/") || strings.ContainsAny(w.Target+w.Source, "\n\x00") {
			return nil, fmt.Errorf("checking container mounts: invalid mount %q from %q", w.Target, w.Source)
		}
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	pid, err := containerPID(ctx, r, ref)
	if err != nil {
		return nil, err
	}
	// Before the reads: a first process in a namespace made inside the
	// container is refused without walking its mounts (the agent could
	// mount anything there, a FUSE file system that never answers too).
	if err := checkRuntimeNamespaces(ctx, r, pid); err != nil {
		return nil, err
	}
	procDir := "/proc/" + strconv.Itoa(pid)
	res, err := runGuestBounded(ctx, r, "head", "-c", strconv.Itoa(liveMountsMaxOutput), procDir+"/mountinfo")
	if err != nil || len(res.Stdout) >= liveMountsMaxOutput {
		return nil, errLiveMounts
	}
	opts := parseMountinfo(res.Stdout)
	if len(opts) == 0 {
		return nil, errLiveMounts
	}
	ids, err := statIDs(ctx, r, procDir, want)
	if err != nil {
		return nil, err
	}
	// After the reads: leaving the runtime's mount namespace is one way
	// (getting back needs a capability the container does not have), so a
	// process in it now was in it for every read above.
	if err := checkRuntimeNamespaces(ctx, r, pid); err != nil {
		return nil, err
	}
	if again, err := containerPID(ctx, r, ref); err != nil || again != pid {
		return nil, errLiveMounts
	}
	var problems []LiveMountProblem
	for _, w := range want {
		o, ok := opts[w.Target]
		switch {
		case !ok:
			problems = append(problems, LiveMountProblem{w.Target, LiveMountMissing})
		case w.ReadOnly && !o.ro:
			problems = append(problems, LiveMountProblem{w.Target, LiveMountWritable})
		case w.SelfMount && !fromParentTree(opts, w.Target, o):
			problems = append(problems, LiveMountProblem{w.Target, LiveMountElsewhere})
		case !sameID(ids, w.Source, procDir+"/root"+w.Target):
			problems = append(problems, LiveMountProblem{w.Target, LiveMountReplaced})
		}
	}
	return problems, nil
}

// fromParentTree reports whether the mount o at target comes from the
// same file system as the nearest mount above it, at the parent's root
// joined with target's path below the parent's point. The kernel records
// the root of a bind mount as the path of the directory it resolved, after
// any symbolic link, so a link that stood anywhere in the source path when
// the runtime mounted, or a directory renamed since (the kernel follows the
// rename, or marks a removed one "//deleted"), shows as another root or
// another device. A target with no mount above it but the container's
// root, or an unreadable root, is no match.
func fromParentTree(opts map[string]mountOpts, target string, o mountOpts) bool {
	if o.root == "" {
		return false
	}
	parent, best := mountOpts{}, ""
	for point, p := range opts {
		if point == "/" || point == target || !strings.HasPrefix(target, strings.TrimSuffix(point, "/")+"/") {
			continue
		}
		if len(point) > len(best) {
			parent, best = p, point
		}
	}
	if best == "" || parent.root == "" || parent.dev != o.dev {
		return false
	}
	rel := strings.TrimPrefix(target, best+"/")
	return o.root == path.Join(parent.root, rel)
}

var pidRE = regexp.MustCompile(`^[0-9]{1,10}$`)

// containerPID is the container's process id in the guest (0 = not
// running).
func containerPID(ctx context.Context, r proc.Runner, ref string) (int, error) {
	res, err := runGuestBounded(ctx, r, "podman", "inspect", "--type", "container", "--format", "{{.State.Pid}}", ref)
	if err != nil {
		if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
			return 0, ErrNoContainer
		}
		return 0, errLiveMounts
	}
	out := strings.TrimSpace(res.Stdout)
	if !pidRE.MatchString(out) {
		return 0, errLiveMounts
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		return 0, errLiveMounts
	}
	if pid <= 0 {
		return 0, ErrContainerNotRunning
	}
	return pid, nil
}

// checkRuntimeNamespaces requires the container's first process (pid, in
// the guest) to be in the mount and user namespaces the container runtime
// created, and not in ones made inside the container.
//
// The threat: a root agent that can ptrace that process can make it setns
// into a user namespace it created (unprivileged, inside the container)
// and a mount namespace owned by it, holding a read-only bind of whatever
// it likes — say a protected directory the host replaced, which the agent
// itself still writes from the real namespace. /proc/<pid>/mountinfo and
// /proc/<pid>/root then show that staged view, and both checks above
// would pass.
//
// The reference this check uses is one that process cannot change: its PID
// namespace. A process never changes its own PID namespace (setns on one
// only affects children), and the runtime created the container's PID
// namespace together with its user namespace, so the PID namespace's owner
// is the container's user namespace for the life of the process. The
// runtime's mount namespace is owned by that same user namespace. A
// namespace made inside the container is owned by a user namespace made
// there, a child of the container's: so the mount namespace's owner must
// equal the PID namespace's owner, and the process must be in that user
// namespace too. (Making a mount namespace owned by the container's own
// user namespace needs CAP_SYS_ADMIN there, which a rootless container is
// not given; an agent with it could unmount a protected path anyway.)
//
// Recording the namespaces at create and comparing later would trust
// whatever ran before the first record (and a resume makes new ones), and
// conmon is in the rootless user and mount namespaces, never the
// container's, so neither is a sound reference.
//
// The guest's perl (nsReader) reads the owners with the NS_GET_USERNS
// ioctl, for this one process only; anything it cannot answer is an error.
func checkRuntimeNamespaces(ctx context.Context, r proc.Runner, pid int) error {
	res, err := runGuestBoundedStdin(ctx, r, strings.NewReader(nsReader), "perl", "-T", "-", strconv.Itoa(pid))
	if err != nil {
		return errLiveMounts
	}
	ns, err := parseNamespaces(res.Stdout)
	if err != nil {
		return err
	}
	owner := ns["pid"].owner
	if owner == 0 || ns["mnt"].owner != owner || ns["user"].id != owner {
		return ErrForeignNamespace
	}
	return nil
}

// nsReader prints "<id> <type> <owner id>" for the user, mnt and pid
// namespaces of the process whose guest pid is its argument (owner 0 for
// the user namespace, whose owner the check does not use), and exits
// non-zero on anything else. It runs as perl from stdin because perl-base
// is Essential on Debian and Ubuntu, so every lever guest has it; under -T,
// which makes perl ignore PERL5LIB and PERL5OPT, and with no module, so
// nothing but the interpreter and this text runs.
//
// Not lsns: lsns walks every process in /proc whatever --task says, and
// aborts on a namespace link it cannot query. On Linux 7.0 the namespace
// link of a same-user process the caller may not ptrace (the run user's
// systemd --user, which holds CAP_WAKE_ALARM) opens as the /proc/<pid>/ns
// directory rather than failing with EACCES, so lsns fails for the run user
// on every call. A container process the run user may not ptrace fails
// here the same way: an error, never "held".
//
// NS_GET_NSTYPE proves each descriptor is a namespace of the expected type
// (a directory or any other file fails it), and all of them must be on the
// one nsfs device. 0xb701 is NS_GET_USERNS and 0xb703 NS_GET_NSTYPE
// (linux/nsfs.h); the types are CLONE_NEWUSER, CLONE_NEWNS, CLONE_NEWPID.
const nsReader = `my $pid = shift;
$pid =~ /\A[1-9][0-9]{0,9}\z/ or die "pid\n";
my %clone = (user => 0x10000000, mnt => 0x00020000, pid => 0x20000000);
my $dev;
for my $t ("user", "mnt", "pid") {
	sysopen(my $ns, "/proc/$pid/ns/$t", 0) or die "open $t\n";
	my $type = ioctl($ns, 0xb703, 0);
	defined $type && $type == $clone{$t} or die "type $t\n";
	my @s = stat($ns) or die "stat $t\n";
	$dev //= $s[0];
	$s[0] == $dev && $s[1] > 0 or die "dev $t\n";
	my $owner = 0;
	if ($t ne "user") {
		my $fd = ioctl($ns, 0xb701, 0);
		defined $fd && $fd > 0 or die "owner $t\n";
		open(my $o, "<&=", $fd) or die "owner fd $t\n";
		my @w = stat($o) or die "owner stat $t\n";
		ioctl($o, 0xb703, 0) == $clone{user} or die "owner type $t\n";
		$w[0] == $dev && $w[1] > 0 or die "owner dev $t\n";
		$owner = $w[1];
		close($o);
	}
	close($ns);
	print "$s[1] $t $owner\n";
}
`

// nsEntry is one namespace line: a namespace's id and its owner's (0 =
// none shown).
type nsEntry struct{ id, owner uint64 }

// parseNamespaces reads nsReader's output, which has the shape of lsns
// --raw --noheadings --output NS,TYPE,ONS. Only the pid, mnt and user lines
// are read, and those strictly: each once, a numeric id and owner, a
// non-zero id. A missing one is an error.
func parseNamespaces(out string) (map[string]nsEntry, error) {
	if len(out) > liveMountsMaxOutput {
		return nil, errLiveMounts
	}
	got := map[string]nsEntry{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[1] {
		case "pid", "mnt", "user":
		default:
			continue
		}
		if len(f) != 3 || !nsNumRE.MatchString(f[0]) || !nsNumRE.MatchString(f[2]) {
			return nil, errLiveMounts
		}
		id, err1 := strconv.ParseUint(f[0], 10, 64)
		owner, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil || id == 0 {
			return nil, errLiveMounts
		}
		if _, dup := got[f[1]]; dup {
			return nil, errLiveMounts
		}
		got[f[1]] = nsEntry{id, owner}
	}
	for _, t := range []string{"pid", "mnt", "user"} {
		if _, ok := got[t]; !ok {
			return nil, errLiveMounts
		}
	}
	return got, nil
}

// NamespaceReader is the program checkRuntimeNamespaces runs as
// perl -T - <pid>, for the live guest test to compare with lsns.
const NamespaceReader = nsReader

var nsNumRE = regexp.MustCompile(`^[0-9]{1,20}$`)

var statLineRE = regexp.MustCompile(`^([0-9]+:[0-9]+):(/.*)$`)

// statIDs is "device:inode" by path for each want's Source and
// container-side Target: one xargs-run stat, the paths on stdin (so none
// of them is a word of the command line), every line checked. stat names
// each path it answers for, and a path it cannot stat (gone, or a link
// that no longer resolves) is simply absent: xargs then exits 123, which
// is not an error here, so an agent that deletes a source cannot turn the
// whole check into "could not read". Any other failure, a line of another
// shape, or a path that was not asked for is an error.
func statIDs(ctx context.Context, r proc.Runner, procDir string, want []LiveMount) (map[string]string, error) {
	var in strings.Builder
	asked := map[string]bool{}
	for _, w := range want {
		for _, p := range []string{w.Source, procDir + "/root" + w.Target} {
			in.WriteString(p)
			in.WriteByte(0)
			asked[p] = true
		}
	}
	res, err := runGuestBoundedStdin(ctx, r, strings.NewReader(in.String()), "xargs", "-0", "stat", "-c", "%d:%i:%n", "--")
	if err != nil && res.Code != 123 {
		return nil, errLiveMounts
	}
	ids := map[string]string{}
	out := strings.TrimSuffix(res.Stdout, "\n")
	if out == "" {
		return ids, nil
	}
	for _, l := range strings.Split(out, "\n") {
		m := statLineRE.FindStringSubmatch(l)
		if m == nil || !asked[m[2]] {
			return nil, errLiveMounts
		}
		ids[m[2]] = m[1]
	}
	return ids, nil
}

// sameID reports whether stat answered for both paths with the same
// device and inode.
func sameID(ids map[string]string, a, b string) bool {
	x, okA := ids[a]
	y, okB := ids[b]
	return okA && okB && x == y
}

// mountOpts is what parseMountinfo keeps of one mount: read-only, the
// device (major:minor) and the root of the mount within its file system
// ("" when the kernel's text could not be read).
type mountOpts struct {
	ro        bool
	dev, root string
}

// parseMountinfo maps each mount point in a /proc/<pid>/mountinfo text to
// its per-mount options, device and root; a later line for the same point (a mount over a
// mount) replaces an earlier one. Only complete lines with the documented
// shape count (proc(5): id parent major:minor root point options
// [optional...] - type source super), and a point is unescaped from the
// kernel's octal form (\040 for a space).
func parseMountinfo(text string) map[string]mountOpts {
	out := map[string]mountOpts{}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	} else {
		return out
	}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !slices.Contains(f[6:], "-") {
			continue
		}
		point, ok := unescapeMountinfo(f[4])
		if !ok {
			continue
		}
		root, ok := unescapeMountinfo(f[3])
		if !ok {
			root = ""
		}
		out[point] = mountOpts{ro: slices.Contains(strings.Split(f[5], ","), "ro"), dev: f[2], root: root}
	}
	return out
}

// unescapeMountinfo decodes the kernel's \ooo escapes in a mountinfo path.
func unescapeMountinfo(s string) (string, bool) {
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", false
		}
		n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", false
		}
		b.WriteByte(byte(n))
		i += 3
	}
	return b.String(), true
}
