package jail

import (
	"context"
	"fmt"
	"io"
	osexec "os/exec"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/exec"
)

// loadImageArgs returns the full host argv (including the prefix binary) that
// loads a docker-archive (read on stdin) into the jail's rootless podman,
// e.g. for the OrbStack prefix:
//
//	orb -m <machine> -u <user> env XDG_RUNTIME_DIR=/run/user/<uid> podman load
//
// Fallback (if pipe proves unreliable): mount-staging — docker save -o
// <tree>/.img.tar on the host, then in-jail: podman load -i <tree>/.img.tar.
// Implement the pipe first.
func loadImageArgs(prefix []string, uid string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "load",
	})
}

// imageInspectArgs returns the host argv that reads the jail podman image ID
// (config digest) for imageRef. The command exits non-zero when the image is
// absent, which the ID readers below treat as "not loaded".
func imageInspectArgs(prefix []string, uid, imageRef string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "image", "inspect", "--format", "{{.Id}}", imageRef,
	})
}

// pruneImagesArgs returns the host argv that prunes DANGLING (untagged,
// unreferenced) images from the jail's rootless podman store. Plain `prune`
// (no `-a`) never removes a tagged image or one still referenced by any
// container, so the running manager — and any stopped worker's image — is
// safe; it only reclaims the layers a rebuilt tag orphaned.
func pruneImagesArgs(prefix []string, uid string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "image", "prune", "-f",
	})
}

// normalizeImageID canonicalizes a docker/podman image ID for comparison. The
// image ID is content-addressed over the image config and so IS identical on
// both sides of a `docker save` | `podman load` (verified empirically) — EXCEPT
// docker prints it with a "sha256:" algorithm prefix and (some) podman versions
// print the bare hex. Stripping the prefix (and surrounding whitespace) on both
// sides is what makes the host-vs-jail comparison actually match; without it the
// guard would never fire.
func normalizeImageID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "sha256:")
}

// hostImageID returns the host docker image ID (config digest) for imageRef,
// normalized (see normalizeImageID), or "" if docker cannot resolve it. Runs on
// the host via the plain exec.Runner seam (not the in-jail JailRunner, which
// would inject jail env and rewrite the argv).
func hostImageID(ctx context.Context, r exec.Runner, imageRef string) string {
	res, err := r.Run(ctx, nil, "docker", "image", "inspect", "--format", "{{.Id}}", imageRef)
	if err != nil {
		return ""
	}
	return normalizeImageID(res.Stdout)
}

// jailImageID returns the jail podman image ID for imageRef, normalized, or ""
// if it is not loaded (the inspect exits non-zero) or the command otherwise
// fails. The prefix argv runs on the host too (the prefix binary reaches into
// the jail), so it takes the same plain host Runner.
func jailImageID(ctx context.Context, r exec.Runner, prefix []string, uid, imageRef string) string {
	args := imageInspectArgs(prefix, uid, imageRef)
	res, err := r.Run(ctx, nil, args[0], args[1:]...)
	if err != nil {
		return ""
	}
	return normalizeImageID(res.Stdout)
}

// ImageLoaded reports whether the jail's rootless podman already holds imageRef
// at the SAME image ID as the host docker image — i.e. the exact bytes are
// present and the multi-GB `docker save | podman load` re-stream can be skipped.
// It is deliberately fail-open: it returns false whenever either ID is
// unavailable (image not yet loaded, docker/podman inspect failure, a rebuilt
// tag whose ID no longer matches), so an unreliable check at worst costs a
// redundant load — never a wrongly-skipped one that would leave a stale image
// in the jail.
func ImageLoaded(ctx context.Context, r exec.Runner, prefix []string, uid, imageRef string) bool {
	host := hostImageID(ctx, r, imageRef)
	if host == "" {
		return false
	}
	return host == jailImageID(ctx, r, prefix, uid, imageRef)
}

// PruneImages removes dangling images from the jail (see pruneImagesArgs). Runs
// on the host via the plain exec.Runner seam. Unlike the removed CombinedOutput
// path, the Runner captures stdout and stderr separately, so the error detail is
// composed from both (stderr first, where podman writes failures).
func PruneImages(ctx context.Context, r exec.Runner, prefix []string, uid string) error {
	args := pruneImagesArgs(prefix, uid)
	res, err := r.Run(ctx, nil, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("prune images: %w: %s", err, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

// LoadImage streams a docker image from the host into the jail's rootless
// podman: `docker save <imageRef>` on the host, piped into loadImageArgs
// through the Runner's stdin seam. Nothing is buffered — the payload can be
// multi-GB.
//
// The producer side (`docker save`) stays on os/exec: the exec.Runner
// contract captures a command's stdout into memory, which is exactly what a
// multi-GB archive must not do, so the one command whose OUTPUT is the stream
// writes straight into the pipe. The consumer side (the jail's `podman load`)
// goes through r.RunStdin like every other in-jail command.
func LoadImage(ctx context.Context, r exec.Runner, prefix []string, uid, imageRef string) error {
	return loadImage(ctx, r, prefix, uid, func(w io.Writer) error {
		save := osexec.CommandContext(ctx, "docker", "save", imageRef)
		save.Stdout = w
		if err := save.Run(); err != nil {
			return fmt.Errorf("docker save: %w", err)
		}
		return nil
	})
}

// loadImage pipes whatever save writes into the jail's `podman load`. save
// runs in its own goroutine so the two ends stream concurrently; its error
// closes the pipe, which the load side observes as a short read. Both errors
// are collected. A load failure is reported first, with podman's stderr: a
// load that dies before draining stdin makes the producer's write fail too,
// and that write error ("closed pipe") is the symptom, not the cause. The
// producer's error is primary only when the load itself succeeded, or when
// the load's own failure is the short read the dead producer caused — in
// which case both are named.
func loadImage(ctx context.Context, r exec.Runner, prefix []string, uid string, save func(io.Writer) error) error {
	pr, pw := io.Pipe()
	saveErr := make(chan error, 1)
	go func() {
		err := save(pw)
		_ = pw.CloseWithError(err)
		saveErr <- err
	}()
	args := loadImageArgs(prefix, uid)
	res, loadErr := r.RunStdin(ctx, pr, nil, args[0], args[1:]...)
	// Unblock a producer still writing after the consumer has gone away.
	_ = pr.CloseWithError(loadErr)
	sErr := <-saveErr
	switch {
	case loadErr != nil && sErr != nil:
		return fmt.Errorf("loadimage: podman load: %w: %s (%v)", loadErr, strings.TrimSpace(res.Stderr), sErr)
	case loadErr != nil:
		return fmt.Errorf("loadimage: podman load: %w: %s", loadErr, strings.TrimSpace(res.Stderr))
	case sErr != nil:
		return fmt.Errorf("loadimage: %w", sErr)
	}
	return nil
}
