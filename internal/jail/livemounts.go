package jail

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// LiveMount is one directory of a container's mount plan, as
// ContainerLiveMounts checks it: Target is the in-container mount point,
// Source the guest path the mount must cover, ReadOnly whether it must be
// mounted read-only.
type LiveMount struct {
	Target, Source string
	ReadOnly       bool
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
)

// liveMountsMaxOutput bounds what ContainerLiveMounts reads from the guest.
const liveMountsMaxOutput = 64 << 10

// errLiveMounts is the one error ContainerLiveMounts returns for anything
// it cannot read or parse: fixed text, so no guest or container output
// reaches a log through it.
var errLiveMounts = errors.New("cannot read the running container's mounts from the guest")

// ErrContainerNotRunning: the container has no process to read mounts from.
var ErrContainerNotRunning = errors.New("the container is not running")

// ContainerLiveMounts checks the running container ref against want from
// the GUEST side, without running a program inside the container (the
// agent is root there and can replace any of them). Through r, as the
// guest user that owns the rootless container:
//
//   - podman inspect gives the container's process id;
//   - /proc/<pid>/mountinfo, which the kernel writes, must list each
//     Target as a mount point (the topmost entry counts), read-only where
//     ReadOnly is set;
//   - stat must find the same device and inode at Source and at
//     /proc/<pid>/root<Target>: a directory replaced on the host leaves
//     the mount on the old one;
//   - the process id is read again at the end and must not have changed.
//
// Every read is bounded in time and size (BoundAgentExec) and parsed
// defensively; anything unexpected is an error, never "held".
func ContainerLiveMounts(ctx context.Context, r proc.Runner, ref string, want []LiveMount) ([]LiveMountProblem, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return nil, fmt.Errorf("checking container mounts: invalid container reference %q", ref)
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
	procDir := "/proc/" + strconv.Itoa(pid)
	res, err := r.Run(ctx, nil, "head", "-c", strconv.Itoa(liveMountsMaxOutput), procDir+"/mountinfo")
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
	if again, err := containerPID(ctx, r, ref); err != nil || again != pid {
		return nil, errLiveMounts
	}
	var problems []LiveMountProblem
	for i, w := range want {
		o, ok := opts[w.Target]
		switch {
		case !ok:
			problems = append(problems, LiveMountProblem{w.Target, LiveMountMissing})
		case w.ReadOnly && !o.ro:
			problems = append(problems, LiveMountProblem{w.Target, LiveMountWritable})
		case ids[2*i] != ids[2*i+1]:
			problems = append(problems, LiveMountProblem{w.Target, LiveMountReplaced})
		}
	}
	return problems, nil
}

var pidRE = regexp.MustCompile(`^[0-9]{1,10}$`)

// containerPID is the container's process id in the guest (0 = not
// running).
func containerPID(ctx context.Context, r proc.Runner, ref string) (int, error) {
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{.State.Pid}}", ref)
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

var statRE = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// statIDs is "device:inode" for each want's Source and container-side
// Target, in that order: one stat call, every line checked.
func statIDs(ctx context.Context, r proc.Runner, procDir string, want []LiveMount) ([]string, error) {
	args := []string{"-c", "%d:%i", "--"}
	for _, w := range want {
		args = append(args, w.Source, procDir+"/root"+w.Target)
	}
	res, err := r.Run(ctx, nil, "stat", args...)
	if err != nil {
		return nil, errLiveMounts
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 2*len(want) {
		return nil, errLiveMounts
	}
	for _, l := range lines {
		if !statRE.MatchString(l) {
			return nil, errLiveMounts
		}
	}
	return lines, nil
}

// mountOpts is what parseMountinfo keeps of one mount.
type mountOpts struct{ ro bool }

// parseMountinfo maps each mount point in a /proc/<pid>/mountinfo text to
// its per-mount options; a later line for the same point (a mount over a
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
		out[point] = mountOpts{ro: slices.Contains(strings.Split(f[5], ","), "ro")}
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
