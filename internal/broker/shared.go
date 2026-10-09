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

// SharedMount is one shared_folders entry as a worker mounts it: Rel is the
// folder relative to the tree (host-side checks), Volume the scion volume
// (in-jail source, /shared/<name> target, read-only unless a writer).
type SharedMount struct {
	Rel    string
	Volume scion.VolumeMount
}

// sharedMountRoot is where shared folders mount in a container
// (config.SharedMountRoot; the broker does not import config).
const sharedMountRoot = "/shared"

// ErrSharedFolder refuses a worker start or resume whose shared folder is
// missing on the host or reached through a symbolic link.
var ErrSharedFolder = errors.New("shared folder is missing or reached through a symbolic link")

// ErrStaleSharedMount refuses to resume a worker whose record holds a
// shared mount the config no longer grants it (removed, or read-write
// where it is now read-only). scion recreates a container from the
// record's volumes, so a resume would hand the old access back.
var ErrStaleSharedMount = errors.New("worker record holds a shared folder mount the config no longer grants")

// verifySharedSources checks each of spec's shared folders on the host: a
// real directory under the tree, reached through no symbolic link. The
// guest resolves a bind source through links, and the tree is
// agent-writable, so a folder swapped for a link would mount whatever the
// link names. A no-op with no tree wired (tests).
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
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("shared folder %q is not a directory: %w", m.Rel, ErrSharedFolder)
		}
	}
	return nil
}

// RecordVolumesFunc reads an agent's volumes from its hub record: what a
// resume recreates the container from.
type RecordVolumesFunc func(ctx context.Context, agent string) ([]scion.VolumeMount, error)

// ErrManagerUnpinned refuses to start or resume a worker with shared
// folders while the running manager does not hold its tree plan (the pins
// over every shared folder and its ancestors): without them the manager
// could swap a folder for a link between lever's check and the moment the
// guest mounts it.
var ErrManagerUnpinned = errors.New("the running manager does not hold its shared folder pins")

// checkSharedAccess is the shared_folders gate before a worker start
// (resume false) or resume: the manager's pins (SharedGuard) when the
// worker mounts any folder, then, for a resume, the record against the
// plan (refuseStaleShares).
func (b *Broker) checkSharedAccess(ctx context.Context, spec WorkerSpec, resume bool) error {
	if len(spec.Shared) > 0 && b.sharedGuard != nil {
		if err := b.sharedGuard(ctx); err != nil {
			return fmt.Errorf("%v: %w", err, ErrManagerUnpinned)
		}
	}
	if resume {
		return b.refuseStaleShares(ctx, spec)
	}
	return nil
}

// sharedAccessHint is the answer text for a refused checkSharedAccess.
func sharedAccessHint(worker string, err error) string {
	if errors.Is(err, ErrManagerUnpinned) {
		return "the running manager does not hold the mounts that protect worker " + worker + "'s shared folders (it was created before they were configured); the operator must back up its conversation and run lever up --fresh"
	}
	return sharedFolderHint(worker)
}

// refuseStaleShares compares the worker's record with its shared_folders
// plan before a resume: every mount the record holds under /shared must be
// one the plan holds, with the same source and no more access. A mount the
// plan adds that the record lacks is not refused (it is less access); the
// worker gets it when it is next created. With no reader wired (tests) it
// does nothing. A failed read refuses the resume whatever the config says
// now: a record outlives the config that made it, so an instance that
// removed every folder may still hold a worker that mounts one.
func (b *Broker) refuseStaleShares(ctx context.Context, spec WorkerSpec) error {
	if b.recordVolumes == nil {
		return nil
	}
	vols, err := b.recordVolumes(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("reading worker %q's record to check its shared folder mounts: %v: %w", spec.Name, err, ErrStaleSharedMount)
	}
	return staleShares(spec, vols)
}

// staleShares is refuseStaleShares' decision.
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

// sharedFolderHint is the fix text for a refused stale record.
func sharedFolderHint(worker string) string {
	return "the worker was created under an older shared_folders plan; the operator must discard its record (lever worker purge " + worker + ", or the manager's agent recycle for a recyclable worker) so it is created again with the current mounts"
}
