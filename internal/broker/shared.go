package broker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/scion"
)

// SharedMount is one shared_folders entry as a worker mounts it: Rel is
// tree-relative for host-side checks, Volume the scion volume.
type SharedMount struct {
	Rel    string
	Volume scion.VolumeMount
}

// sharedMountRoot mirrors config.SharedMountRoot (no config import).
const sharedMountRoot = "/shared"

// ErrSharedFolder refuses a worker start or resume whose shared folder is
// missing on the host or reached through a symbolic link.
var ErrSharedFolder = errors.New("shared folder is missing or reached through a symbolic link")

// ErrStaleSharedMount refuses to resume a worker whose record holds a shared
// mount the config no longer grants: scion recreates the container from the
// record's volumes, so a resume would hand the old access back.
var ErrStaleSharedMount = errors.New("worker record holds a shared folder mount the config no longer grants")

// verifySharedSources requires each shared folder to be a real directory
// under the tree, reached through no symlink: the tree is agent-writable and
// the guest resolves a bind source through links.
func (b *Broker) verifySharedSources(spec WorkerSpec) error {
	if b.tree == "" {
		return nil
	}
	for _, m := range spec.Shared {
		if err := refuseEscapingDir(b.tree, filepath.FromSlash(m.Rel), true); err != nil {
			return fmt.Errorf("shared folder %q: %v: %w", m.Rel, err, ErrSharedFolder)
		}
		fi, err := os.Lstat(filepath.Join(b.tree, filepath.FromSlash(m.Rel)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("shared folder %q does not exist on the host: %w", m.Rel, ErrSharedFolder)
			}
			return fmt.Errorf("shared folder %q: %v: %w", m.Rel, err, ErrSharedFolder)
		}
		if !fi.IsDir() {
			return fmt.Errorf("shared folder %q is not a directory: %w", m.Rel, ErrSharedFolder)
		}
	}
	return nil
}

// RecordVolumesFunc reads an agent's volumes from its hub record.
type RecordVolumesFunc func(ctx context.Context, agent string) ([]scion.VolumeMount, error)

// ErrManagerUnpinned refuses a worker start or resume while the running
// manager does not hold its tree pins: without them the manager could swap a
// folder for a link between lever's check and the guest's mount.
var ErrManagerUnpinned = errors.New("the running manager does not hold its shared folder pins")

// ErrSharedCheckUnavailable: the check could not read what it needs. The
// start or resume is refused, but nothing is known to be wrong: retry.
var ErrSharedCheckUnavailable = errors.New("lever could not complete the shared folder check")

// checkSharedAccess gates a worker start or resume on the manager's pins,
// for every worker (a worker dir swapped for a link to a folder would mount
// it read-write), and on a resume also checks the record against the plan.
func (b *Broker) checkSharedAccess(ctx context.Context, spec WorkerSpec, resume bool) error {
	if b.sharedGuard != nil {
		if err := b.sharedGuard(ctx); err != nil {
			if errors.Is(err, ErrSharedCheckUnavailable) {
				return err
			}
			return fmt.Errorf("%v: %w", err, ErrManagerUnpinned)
		}
	}
	if resume {
		return b.refuseStaleShares(ctx, spec)
	}
	return nil
}

func sharedAccessHint(worker string, err error) string {
	switch {
	case errors.Is(err, ErrSharedCheckUnavailable):
		return "lever could not read what it needs to check worker " + worker + "'s shared folder access (the hub record or the manager's mounts); retry, and if it persists ask the operator to run lever doctor"
	case errors.Is(err, ErrManagerUnpinned):
		return "the running manager does not hold the mounts that protect the shared folders and worker dirs (it was created before the current plan, or a protected directory was replaced on the host); the operator should run lever doctor, then run lever up --fresh --keep-conversation"
	}
	return sharedFolderHint(worker)
}

// refuseStaleShares requires every /shared mount in the record to be one the
// plan holds, with the same source and no more access. A mount the record
// lacks is less access and is not refused.
//
// While folders are configured a failed read refuses (fail closed); with
// none, a failed read is ignored so a hub hiccup cannot block every resume.
func (b *Broker) refuseStaleShares(ctx context.Context, spec WorkerSpec) error {
	switch {
	case b.recordVolumes != nil:
		vols, err := b.recordVolumes(ctx, spec.Name)
		if err != nil {
			return fmt.Errorf("reading worker %q's record to check its shared folder mounts: %v: %w", spec.Name, err, ErrSharedCheckUnavailable)
		}
		return staleShares(spec, vols)
	case b.leftoverShareVolumes != nil:
		vols, err := b.leftoverShareVolumes(ctx, spec.Name)
		if err != nil {
			return nil
		}
		return staleShares(spec, vols)
	}
	return nil
}

func staleShares(spec WorkerSpec, vols []scion.VolumeMount) error {
	var stale []string
	for _, v := range vols {
		t := path.Clean(v.Target)
		if t != sharedMountRoot && !strings.HasPrefix(t, sharedMountRoot+"/") {
			continue
		}
		ok := false
		for _, m := range spec.Shared {
			if path.Clean(m.Volume.Target) == t && path.Clean(m.Volume.Source) == path.Clean(v.Source) && (v.ReadOnly || !m.Volume.ReadOnly) {
				ok = true
				break
			}
		}
		if !ok {
			mode := "read-only"
			if !v.ReadOnly {
				mode = "read-write"
			}
			stale = append(stale, t+" ("+mode+")")
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("worker %q: %s: %w", spec.Name, strings.Join(stale, ", "), ErrStaleSharedMount)
	}
	return nil
}

func sharedFolderHint(worker string) string {
	return "the worker was created under an older shared_folders plan; the operator must discard its record (lever worker purge " + worker + ", or the manager's agent recycle for a recyclable worker) so it is created again with the current mounts"
}
