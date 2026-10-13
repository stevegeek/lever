package jail

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

func loadImageArgs(prefix []string, uid string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "load",
	})
}

func imageInspectArgs(prefix []string, uid, imageRef string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "image", "inspect", "--format", "{{.Id}}", imageRef,
	})
}

// pruneImagesArgs: plain `prune` (no -a) never removes a tagged image or one
// a container references, so the manager's and stopped workers' images are
// safe.
func pruneImagesArgs(prefix []string, uid string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "image", "prune", "-f",
	})
}

// ScionKeepIDUserns is the user-namespace flag scion's rootless podman runtime
// starts every agent container with (pkg/runtime/podman.go at the pinned
// scion). A warm-up run must use exactly this flag, because podman keeps one
// id-mapped layer copy per mapping.
const ScionKeepIDUserns = "--userns=keep-id:uid=1000,gid=1000"

// warmImageArgs: the entrypoint is `true` so neither sciontool's hook nor
// claude runs.
func warmImageArgs(prefix []string, uid, imageRef string) []string {
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "run", "--rm", "--pull=never", "--network=none",
		ScionKeepIDUserns, "--entrypoint", "true", imageRef,
	})
}

// normalizeImageID: the image ID is the same on both sides of docker save |
// podman load, but docker prints a "sha256:" prefix and some podman versions
// print the bare hex.
func normalizeImageID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "sha256:")
}

// hostImageID takes a plain host Runner, not a JailRunner, which would
// rewrite the argv into the jail.
func hostImageID(ctx context.Context, r proc.Runner, imageRef string) string {
	res, err := r.Run(ctx, nil, "docker", "image", "inspect", "--format", "{{.Id}}", imageRef)
	if err != nil {
		return ""
	}
	return normalizeImageID(res.Stdout)
}

// jailImageID takes a plain host Runner: the prefix binary reaches into the
// jail itself.
func jailImageID(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef string) string {
	args := imageInspectArgs(prefix, uid, imageRef)
	res, err := r.Run(ctx, nil, args[0], args[1:]...)
	if err != nil {
		return ""
	}
	return normalizeImageID(res.Stdout)
}

// ImageLoaded reports whether the jail's podman already holds imageRef at the
// same image ID as the host docker image, so the load can be skipped. It
// returns false whenever either ID is unavailable: a bad check costs a
// redundant load, never a skipped one that leaves a stale image in the jail.
func ImageLoaded(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef string) bool {
	host := hostImageID(ctx, r, imageRef)
	if host == "" {
		return false
	}
	return host == jailImageID(ctx, r, prefix, uid, imageRef)
}

// PruneImages removes dangling images from the jail (see pruneImagesArgs).
func PruneImages(ctx context.Context, r proc.Runner, prefix []string, uid string) error {
	args := pruneImagesArgs(prefix, uid)
	res, err := r.Run(ctx, nil, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("prune images: %w: %s", err, podmanFailure(res.Stderr, res.Stdout))
	}
	return nil
}

// WarmImage makes rootless podman build the id-mapped layer copy of imageRef
// for scion's keep-id mapping. The first container under a mapping copies
// every layer, which takes most of a minute for a multi-GB image, while
// scion's agent start is cancelled after about 30 s and the partial work is
// deleted. A run on an already warm image is fast, so nothing is detected
// first.
func WarmImage(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef string) error {
	args := warmImageArgs(prefix, uid, imageRef)
	res, err := r.Run(ctx, nil, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("warm image %s: %w: %s", imageRef, err, podmanFailure(res.Stderr, res.Stdout))
	}
	return nil
}

// LoadImage streams `docker save <imageRef>` from the host into the jail's
// rootless podman. docker save stays on os/exec because proc.Runner captures
// stdout into memory, which a multi-GB archive must not do.
func LoadImage(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef string) error {
	return loadImageAndAlias(ctx, r, prefix, uid, imageRef, func(w io.Writer) error {
		save := exec.CommandContext(ctx, "docker", "save", imageRef)
		save.Stdout = w
		if err := save.Run(); err != nil {
			return fmt.Errorf("docker save: %w", err)
		}
		return nil
	})
}

func loadImageAndAlias(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef string, save func(io.Writer) error) error {
	if err := loadImage(ctx, r, prefix, uid, save); err != nil {
		return err
	}
	return aliasLocalhost(ctx, r, prefix, uid, imageRef)
}

// localhostAliasArgs: ref must be unqualified (see aliasLocalhost).
func localhostAliasArgs(prefix []string, uid, ref string) []string {
	tagged := ref
	if !strings.ContainsAny(ref[strings.LastIndex(ref, "/")+1:], ":@") {
		tagged += ":latest"
	}
	src := "docker.io/" + tagged
	if !strings.Contains(tagged, "/") {
		src = "docker.io/library/" + tagged
	}
	return slices.Concat(prefix, []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "tag", src, "localhost/" + tagged,
	})
}

// aliasLocalhost: `podman load` of a docker archive names the image
// docker.io/<RepoTag>, while the container spec's unqualified <ref> resolves
// through podman's short-name path to localhost/<ref>, a name an earlier load
// left behind, so after a rebuild the container would run the old image.
// Tagging the fresh image as localhost/ makes both names one image. A ref
// that names its registry is loaded under that exact name, and a
// digest-pinned ref has no RepoTag and cannot be a tag target. Failure is
// fatal so the stale image is never run silently.
func aliasLocalhost(ctx context.Context, r proc.Runner, prefix []string, uid, ref string) error {
	if strings.Contains(ref, "@") {
		return nil
	}
	if i := strings.Index(ref, "/"); i > 0 {
		if first := ref[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
			return nil
		}
	}
	args := localhostAliasArgs(prefix, uid, ref)
	res, err := r.Run(ctx, nil, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("loadimage: tagging %s as %s: %w: %s", args[len(args)-2], args[len(args)-1], err, podmanFailure(res.Stderr, res.Stdout))
	}
	return nil
}

// loadImage pipes whatever save writes into the jail's `podman load`. A load
// failure is reported first, with podman's stderr: a load that dies before
// draining stdin makes the producer's write fail too ("closed pipe"), which
// is the symptom, not the cause.
func loadImage(ctx context.Context, r proc.Runner, prefix []string, uid string, save func(io.Writer) error) error {
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
		return fmt.Errorf("loadimage: podman load: %w: %s (%v)", loadErr, podmanFailure(res.Stderr, ""), sErr)
	case loadErr != nil:
		return fmt.Errorf("loadimage: podman load: %w: %s", loadErr, podmanFailure(res.Stderr, ""))
	case sErr != nil:
		return fmt.Errorf("loadimage: %w", sErr)
	}
	return nil
}

// LoadImageTar streams a shipped docker archive from the host into the jail's
// rootless podman, never touching the host docker store. The archive must
// carry imageRef, checked before a byte is streamed.
//
// allowTag, when non-nil, is applied to every RepoTag the archive carries,
// not just imageRef: `podman load` imports every image in the archive, so an
// image from a registry the config does not allow would otherwise land in the
// jail store unchecked. An image with no RepoTags loads untagged, and the
// dangling-image prune that follows a load removes it.
func LoadImageTar(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef, tarPath string, allowTag func(ref string) error) error {
	imgs, err := ReadImageTar(tarPath)
	if err != nil {
		return err
	}
	if _, err := FindTarImage(imgs, imageRef); err != nil {
		return fmt.Errorf("image tar %s: %w", tarPath, err)
	}
	if allowTag != nil {
		for _, img := range imgs {
			for _, t := range img.RepoTags {
				if err := allowTag(t); err != nil {
					return fmt.Errorf("image tar %s: refusing to load: it carries tag %q, and %w", tarPath, t, err)
				}
			}
		}
	}
	return loadImageAndAlias(ctx, r, prefix, uid, imageRef, func(w io.Writer) error {
		f, err := os.Open(tarPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return fmt.Errorf("reading %s: %w", tarPath, err)
		}
		return nil
	})
}

// ImageLoadedTar is ImageLoaded for a shipped archive: the archive's config
// digest is the ID `podman load` assigns. Like ImageLoaded it returns false
// on any read failure.
func ImageLoadedTar(ctx context.Context, r proc.Runner, prefix []string, uid, imageRef, tarPath string) bool {
	imgs, err := ReadImageTar(tarPath)
	if err != nil {
		return false
	}
	img, err := FindTarImage(imgs, imageRef)
	if err != nil {
		return false
	}
	return img.ConfigDigest == jailImageID(ctx, r, prefix, uid, imageRef)
}
