package jail

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// ContainerState is podman's own view of one container: its full id, its
// name and its state.
type ContainerState struct {
	ID      string
	Name    string
	Status  string // podman's State.Status: created, running, paused, exited, ...
	Running bool
	Paused  bool
}

// Removable reports whether podman's state is one with no process in the
// container: created, configured, initialized, exited or stopped. Every other
// state, and any state with Running or Paused set, is not.
func (s ContainerState) Removable() bool {
	if s.Running || s.Paused {
		return false
	}
	return slices.Contains([]string{"created", "configured", "initialized", "exited", "stopped"}, s.Status)
}

var containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// InspectContainerState reads ref's state from podman. It returns
// ErrNoContainer when podman knows no such container, and refuses an answer
// whose name is not ref, so a reference podman resolved some other way (an id
// prefix) is never acted on.
func InspectContainerState(ctx context.Context, r proc.Runner, ref string) (ContainerState, error) {
	if err := checkRef(ref); err != nil {
		return ContainerState{}, fmt.Errorf("inspecting container state: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, containerStateTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{.Id}} {{.Name}} {{.State.Status}} {{.State.Running}} {{.State.Paused}}", ref)
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return ContainerState{}, fmt.Errorf("inspecting container %s: %w", ref, ErrNoContainer)
		}
		return ContainerState{}, fmt.Errorf("inspecting container %s state: %w: %s", ref, err, strings.TrimSpace(res.Stderr))
	}
	f := strings.Fields(res.Stdout)
	if len(f) != 5 || !containerIDRE.MatchString(f[0]) || f[1] != ref {
		return ContainerState{}, fmt.Errorf("inspecting container %s state: unexpected answer", ref)
	}
	running, rerr := strconv.ParseBool(f[3])
	paused, perr := strconv.ParseBool(f[4])
	if rerr != nil || perr != nil {
		return ContainerState{}, fmt.Errorf("inspecting container %s state: unexpected answer", ref)
	}
	return ContainerState{ID: f[0], Name: f[1], Status: strings.ToLower(f[2]), Running: running, Paused: paused}, nil
}

// containerSizeTimeout bounds the writable-layer measurement: podman walks
// the whole layer, which takes a while for a large one.
var containerSizeTimeout = time.Minute

// ContainerWritableSize is the size in bytes of the writable layer of the
// container with full id id.
func ContainerWritableSize(ctx context.Context, r proc.Runner, id string) (int64, error) {
	if !containerIDRE.MatchString(id) {
		return 0, fmt.Errorf("measuring container layer: invalid container id %q", id)
	}
	ctx, cancel := context.WithTimeout(ctx, containerSizeTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "container", "inspect", "--size", "--format", "{{json .SizeRw}}", id)
	if err != nil {
		return 0, fmt.Errorf("measuring container %.12s layer: %w: %s", id, err, strings.TrimSpace(res.Stderr))
	}
	var n *int64
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &n); err != nil || n == nil || *n < 0 {
		return 0, fmt.Errorf("measuring container %.12s layer: unexpected answer", id)
	}
	return *n, nil
}

// backupRepoRE is the repository part of a backup image name: podman
// refuses upper case, and the name must not read as a flag.
var backupRepoRE = regexp.MustCompile(`^lever\.local/[a-z0-9][a-z0-9_.-]*-backup$`)

// BackupImageRepo is the repository the backups of container name are
// committed to, or an error when name cannot form one.
func BackupImageRepo(name string) (string, error) {
	repo := "lever.local/" + name + "-backup"
	if !backupRepoRE.MatchString(repo) || strings.Contains(name, "..") {
		return "", fmt.Errorf("container name %q cannot name a backup image", name)
	}
	return repo, nil
}

// containerCommitTimeout bounds a commit, which copies the whole writable
// layer.
var containerCommitTimeout = 20 * time.Minute

// CommitContainer commits the container with full id id to image.
func CommitContainer(ctx context.Context, r proc.Runner, id, image string) error {
	if !containerIDRE.MatchString(id) {
		return fmt.Errorf("committing container: invalid container id %q", id)
	}
	repo, _, ok := strings.Cut(image, ":")
	if !ok || !backupRepoRE.MatchString(repo) {
		return fmt.Errorf("committing container: invalid backup image %q", image)
	}
	ctx, cancel := context.WithTimeout(ctx, containerCommitTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "commit", "--quiet", id, image)
	if err != nil {
		return fmt.Errorf("committing container %.12s to %s: %w: %s", id, image, err, podmanErrTail(res.Stderr))
	}
	return nil
}

// RemoveStoppedContainer removes the container with full id id. It never
// forces: podman refuses to remove a running or paused container, which is
// the last guard against removing one that started since it was inspected.
// A container already gone is not an error.
func RemoveStoppedContainer(ctx context.Context, r proc.Runner, id string) error {
	if !containerIDRE.MatchString(id) {
		return fmt.Errorf("removing container: invalid container id %q", id)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "rm", id)
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return nil
		}
		return fmt.Errorf("removing container %.12s: %w: %s", id, err, podmanErrTail(res.Stderr))
	}
	return nil
}

// PruneBackupImages removes all but the keep newest images of repo (tags
// sort by time) and returns the ones it removed. A failed removal is
// reported and the rest still tried.
func PruneBackupImages(ctx context.Context, r proc.Runner, repo string, keep int) ([]string, error) {
	if !backupRepoRE.MatchString(repo) {
		return nil, fmt.Errorf("pruning backups: invalid backup repository %q", repo)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+repo)
	if err != nil {
		return nil, fmt.Errorf("listing backup images: %w: %s", err, strings.TrimSpace(res.Stderr))
	}
	var tags []string
	for line := range strings.Lines(res.Stdout) {
		line = strings.TrimSpace(line)
		if t, ok := strings.CutPrefix(line, repo+":"); ok && backupTagRE.MatchString(t) {
			tags = append(tags, t)
		}
	}
	slices.Sort(tags)
	slices.Reverse(tags)
	var removed []string
	var errs []string
	for _, t := range tags[min(keep, len(tags)):] {
		img := repo + ":" + t
		if res, err := r.Run(ctx, nil, "podman", "rmi", img); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", img, podmanErrTail(res.Stderr)))
			continue
		}
		removed = append(removed, img)
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("removing old backup images: %s", strings.Join(errs, "; "))
	}
	return removed, nil
}

// BackupTag is the tag of a backup taken at t: UTC, so tags sort by time.
func BackupTag(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

var backupTagRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// ContainerJanitor runs the functions above through R, podman as the jail
// user.
type ContainerJanitor struct{ R proc.Runner }

func (j ContainerJanitor) State(ctx context.Context, ref string) (ContainerState, error) {
	return InspectContainerState(ctx, j.R, ref)
}

func (j ContainerJanitor) WritableSize(ctx context.Context, id string) (int64, error) {
	return ContainerWritableSize(ctx, j.R, id)
}

func (j ContainerJanitor) Commit(ctx context.Context, id, image string) error {
	return CommitContainer(ctx, j.R, id, image)
}

func (j ContainerJanitor) Remove(ctx context.Context, id string) error {
	return RemoveStoppedContainer(ctx, j.R, id)
}

func (j ContainerJanitor) PruneBackups(ctx context.Context, repo string, keep int) ([]string, error) {
	return PruneBackupImages(ctx, j.R, repo, keep)
}
