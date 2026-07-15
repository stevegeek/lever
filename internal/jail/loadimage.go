package jail

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// LoadImageArgs returns the full host argv (including the prefix binary) that
// loads a docker-archive (read on stdin) into the jail's rootless podman,
// e.g. for the OrbStack prefix:
//
//	orb -m <machine> -u <user> env XDG_RUNTIME_DIR=/run/user/<uid> podman load
//
// Fallback (if pipe proves unreliable): mount-staging — docker save -o
// <tree>/.img.tar on the host, then in-jail: podman load -i <tree>/.img.tar.
// Implement the pipe first.
func LoadImageArgs(prefix []string, uid string) []string {
	argv := append([]string{}, prefix...)
	return append(argv,
		"env",
		"XDG_RUNTIME_DIR=/run/user/"+uid,
		"podman", "load",
	)
}

// ImageInspectArgs returns the host argv that reads the jail podman image ID
// (config digest) for imageRef. The command exits non-zero when the image is
// absent, which the ID readers below treat as "not loaded".
func ImageInspectArgs(prefix []string, uid, imageRef string) []string {
	argv := append([]string{}, prefix...)
	return append(argv,
		"env",
		"XDG_RUNTIME_DIR=/run/user/"+uid,
		"podman", "image", "inspect", "--format", "{{.Id}}", imageRef,
	)
}

// PruneImagesArgs returns the host argv that prunes DANGLING (untagged,
// unreferenced) images from the jail's rootless podman store. Plain `prune`
// (no `-a`) never removes a tagged image or one still referenced by any
// container, so the running manager — and any stopped worker's image — is
// safe; it only reclaims the layers a rebuilt tag orphaned.
func PruneImagesArgs(prefix []string, uid string) []string {
	argv := append([]string{}, prefix...)
	return append(argv,
		"env",
		"XDG_RUNTIME_DIR=/run/user/"+uid,
		"podman", "image", "prune", "-f",
	)
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
// normalized (see normalizeImageID), or "" if docker cannot resolve it.
func hostImageID(ctx context.Context, imageRef string) string {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", imageRef).Output()
	if err != nil {
		return ""
	}
	return normalizeImageID(string(out))
}

// jailImageID returns the jail podman image ID for imageRef, normalized, or ""
// if it is not loaded (the inspect exits non-zero) or the command otherwise
// fails.
func jailImageID(ctx context.Context, prefix []string, uid, imageRef string) string {
	args := ImageInspectArgs(prefix, uid, imageRef)
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if err != nil {
		return ""
	}
	return normalizeImageID(string(out))
}

// ImageLoaded reports whether the jail's rootless podman already holds imageRef
// at the SAME image ID as the host docker image — i.e. the exact bytes are
// present and the multi-GB `docker save | podman load` re-stream can be skipped.
// It is deliberately fail-open: it returns false whenever either ID is
// unavailable (image not yet loaded, docker/podman inspect failure, a rebuilt
// tag whose ID no longer matches), so an unreliable check at worst costs a
// redundant load — never a wrongly-skipped one that would leave a stale image
// in the jail.
func ImageLoaded(ctx context.Context, prefix []string, uid, imageRef string) bool {
	host := hostImageID(ctx, imageRef)
	if host == "" {
		return false
	}
	return host == jailImageID(ctx, prefix, uid, imageRef)
}

// PruneImages removes dangling images from the jail (see PruneImagesArgs).
func PruneImages(ctx context.Context, prefix []string, uid string) error {
	args := PruneImagesArgs(prefix, uid)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("prune images: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LoadImage streams a docker image from the host into the jail's rootless
// podman by piping `docker save <imageRef>` into the backend's LoadImageArgs
// argv. Uses os/exec directly because the payload can be multi-GB — the
// exec.Runner abstraction buffers stdout in memory, which is unsuitable here.
func LoadImage(ctx context.Context, prefix []string, uid, imageRef string) error {
	save := exec.CommandContext(ctx, "docker", "save", imageRef)
	args := LoadImageArgs(prefix, uid)
	load := exec.CommandContext(ctx, args[0], args[1:]...)

	pipe, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("loadimage: stdout pipe: %w", err)
	}
	load.Stdin = pipe

	if err := save.Start(); err != nil {
		return fmt.Errorf("loadimage: docker save start: %w", err)
	}
	if err := load.Start(); err != nil {
		_ = save.Process.Kill()
		return fmt.Errorf("loadimage: jail podman load start: %w", err)
	}

	// Wait for both; collect errors from both sides.
	saveErr := save.Wait()
	loadErr := load.Wait()

	if saveErr != nil {
		return fmt.Errorf("loadimage: docker save: %w", saveErr)
	}
	if loadErr != nil {
		return fmt.Errorf("loadimage: podman load: %w", loadErr)
	}
	return nil
}
