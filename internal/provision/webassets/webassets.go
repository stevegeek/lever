// Package webassets builds scion's web UI (the vite SPA under scion's web/)
// on the host and returns a finished dist/client directory, cached per source
// digest. The guest carries no node, and giving it one would also give the
// jail npm's egress, so the registry traffic stays on the host.
package webassets

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// webAssetsExclude drops vite's sourcemaps (most of the build output) from
// the staged payload: a filepath.Match pattern against each file's base name
// (see WriteTar).
const webAssetsExclude = "*.map"

// BuildMarker is written into a host build directory as the last step of a
// successful build, so an interrupted build is visibly incomplete and gets
// rebuilt. It holds the digest it was built from.
const BuildMarker = ".lever-web-build"

// minNodeMajor mirrors scion's web/package.json `engines.node`. npm only
// warns about an engines mismatch, and an old node otherwise fails with a
// syntax error from deep inside vite.
const minNodeMajor = 20

// ErrNodeToolchain means this host cannot build scion's web UI; the apply
// path and `lever doctor` share it for one diagnosis.
var ErrNodeToolchain = errors.New("node/npm toolchain not usable")

// NodeToolchainFix is the remediation printed for ErrNodeToolchain. It names
// the asdf/mise shim case: a shim on PATH whose version is not installed
// exits 126 with no useful text.
const NodeToolchainFix = `put a REAL node+npm on PATH (not just an asdf/mise shim), e.g. export PATH="$HOME/.asdf/installs/nodejs/<ver>/bin:$PATH"; ` +
	"`node --version` should print"

// SourceDir resolves the host directory holding scion's npm project (the
// module's or checkout's `web/`). The Version branch reuses
// scionbin.FetchModule, so the SPA is built from the same source tree as the
// pinned binary. That `go mod download` asks the module proxy each time for a
// commit-hash pin, so callers look for PinnedBuild first.
func SourceDir(ctx context.Context, r proc.Runner, spec scionbin.Spec) (string, error) {
	root := spec.Source
	if spec.Version != "" {
		_, dir, err := scionbin.FetchModule(ctx, r, spec.Version)
		if err != nil {
			return "", err
		}
		root = dir
	}
	if root == "" {
		// Unreachable via Spec.BuildsWebAssets.
		return "", errors.New("no scion source to build web assets from")
	}
	web := filepath.Join(root, "web")
	if _, err := os.Stat(filepath.Join(web, "package.json")); err != nil {
		return "", fmt.Errorf("scion web sources not found at %s (no package.json): %w", web, err)
	}
	return web, nil
}

// CacheRoot is the host directory holding per-pin scion web builds: a user
// cache directory, not shared /tmp, since it holds a node_modules per pin
// that a tmp sweep would force to re-download. `lever doctor` probes the
// node toolchain from this same directory (CheckNodeToolchain).
func CacheRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(cache, "lever", "scion-web"), nil
}

// CheckNodeToolchain verifies a real, working node+npm is resolvable for the
// web build, and returns the node version. A version manager that resolves
// node by walking up from the working directory (asdf, mise) can answer
// differently per directory, so callers probe in the build's own directory.
func CheckNodeToolchain(ctx context.Context, r proc.Runner, probeDir string) (string, error) {
	res, err := r.RunIn(ctx, probeDir, nil, "node", "--version")
	if err != nil {
		return "", fmt.Errorf("%w: node --version: %v", ErrNodeToolchain, err)
	}
	version := strings.TrimSpace(res.Stdout)
	major, err := nodeMajor(version)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNodeToolchain, err)
	}
	if major < minNodeMajor {
		return "", fmt.Errorf("%w: node %s is too old — scion's web UI needs node >= %d (its package.json engines)",
			ErrNodeToolchain, version, minNodeMajor)
	}
	if _, err := r.RunIn(ctx, probeDir, nil, "npm", "--version"); err != nil {
		return "", fmt.Errorf("%w: npm --version: %v", ErrNodeToolchain, err)
	}
	return version, nil
}

func nodeMajor(out string) (int, error) {
	v := strings.TrimSpace(out)
	v = strings.TrimPrefix(v, "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("could not parse a node version from %q", strings.TrimSpace(out))
	}
	return n, nil
}

// Build produces a built dist/client on the host and returns its path and
// the source digest. It builds only when the cache does not already hold a
// complete build for that digest.
func Build(ctx context.Context, r proc.Runner, srcWeb string) (dist, digest string, err error) {
	digest, err = HashSource(srcWeb)
	if err != nil {
		return "", "", fmt.Errorf("hashing scion web sources at %s: %w", srcWeb, err)
	}
	root, err := CacheRoot()
	if err != nil {
		return "", "", err
	}
	buildDir := filepath.Join(root, digest)
	dist = filepath.Join(buildDir, "dist", "client")
	if webBuildComplete(buildDir) {
		return dist, digest, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", fmt.Errorf("create web build cache %s: %w", root, err)
	}
	// Probe before anything is written: a missing toolchain is the common
	// first-run failure. root has the build directory's ancestry, so a walk-up
	// version manager resolves identically.
	if _, err := CheckNodeToolchain(ctx, r, root); err != nil {
		return "", "", fmt.Errorf("%w\n    fix: %s", err, NodeToolchainFix)
	}
	// Build in a private directory and rename it into place:
	//
	//   - Two instances on one host share this cache; building in place would
	//     let one's RemoveAll delete the other's node_modules mid-install. The
	//     rename is atomic, so the worst case is wasted work.
	//   - npm's copy:client writes build output back into public/, which vite
	//     copies into the next dist, so a reused directory accumulates old
	//     assets.
	staging, err := os.MkdirTemp(root, ".build-")
	if err != nil {
		return "", "", fmt.Errorf("create web build staging dir under %s: %w", root, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// The Go module cache is read-only, so the npm project must be copied
	// somewhere writable.
	if err := copyTree(srcWeb, staging, skipWebSourcePath); err != nil {
		return "", "", fmt.Errorf("stage scion web sources for build: %w", err)
	}
	// `npm ci` installs exactly package-lock.json and fails if the lock and
	// manifest disagree, so a pin builds the same way twice.
	if _, err := r.RunIn(ctx, staging, nil, "npm", "ci", "--no-audit", "--no-fund"); err != nil {
		return "", "", fmt.Errorf("npm ci for scion web assets in %s: %w", staging, err)
	}
	if _, err := r.RunIn(ctx, staging, nil, "npm", "run", "build"); err != nil {
		return "", "", fmt.Errorf("npm run build for scion web assets in %s: %w", staging, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "dist", "client", filepath.FromSlash(layout.WebAssetsSentinel))); err != nil {
		return "", "", fmt.Errorf("scion web build produced no %s — the build reported success but its output is unusable", layout.WebAssetsSentinel)
	}
	if err := publishBuild(staging, buildDir, digest); err != nil {
		return "", "", err
	}
	return dist, digest, nil
}

// publishBuild marks a finished build in staging complete and moves it to
// buildDir.
func publishBuild(staging, buildDir, digest string) error {
	// Marker before the rename: nothing may observe a half-built cache.
	if err := os.WriteFile(filepath.Join(staging, BuildMarker), []byte(digest), 0o644); err != nil {
		return fmt.Errorf("marking scion web build complete: %w", err)
	}
	if err := os.Rename(staging, buildDir); err != nil {
		// Another build finished first; same digest, same output, so adopt it.
		if webBuildComplete(buildDir) {
			return nil
		}
		// Otherwise the destination holds something incomplete: clear it and
		// retry once.
		if rmErr := os.RemoveAll(buildDir); rmErr != nil {
			return fmt.Errorf("clear incomplete web build %s: %w", buildDir, rmErr)
		}
		if err := os.Rename(staging, buildDir); err != nil {
			return fmt.Errorf("publish web build to %s: %w", buildDir, err)
		}
	}
	return nil
}

// webBuildComplete requires both the completion marker and the asset scion
// serves: the marker alone survives someone clearing dist/, and the asset
// alone would accept a build interrupted after vite.
func webBuildComplete(buildDir string) bool {
	if _, err := os.Stat(filepath.Join(buildDir, BuildMarker)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(buildDir, "dist", "client", filepath.FromSlash(layout.WebAssetsSentinel)))
	return err == nil
}

// skipWebSourcePath reports whether a path relative to scion's web/ is build
// output, excluded from both the digest and the copy. npm's postinstall and
// copy:client generate public/assets and public/shoelace inside the source
// tree, so hashing them would change the digest on every build.
func skipWebSourcePath(rel string) bool {
	switch rel {
	case "node_modules", "dist", "public/assets", "public/shoelace":
		return true
	}
	return false
}

// HashSource digests the build inputs under root: every regular file's
// relative path and contents, in WalkDir's lexical order. Contents, not the
// pin, so an edited `source:` checkout is never served a stale UI. NUL
// separators keep path and content boundaries unambiguous.
func HashSource(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skipWebSourcePath(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if _, err := fmt.Fprintf(h, "%s\x00", rel); err != nil {
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		_, err = h.Write([]byte{0})
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyTree copies the regular files under src into dst, skipping any path
// skip reports. Modes are normalised (0755 dirs, 0644 files): the source is
// usually the read-only module cache, and npm must write into its project.
func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// WriteTar writes dist as a tar archive to w: regular files and symlinks,
// paths relative to dist, sourcemaps dropped. Written by lever, not the
// host's tar, so the archive is the same from every host: no macOS
// AppleDouble `._name` members and no host uid/gid (the guest extracts as
// root with --no-same-owner).
func WriteTar(w io.Writer, dist string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dist, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dist, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777})
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s: not a regular file", path)
		}
		if skip, _ := filepath.Match(webAssetsExclude, d.Name()); skip {
			return nil
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size()}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return fmt.Errorf("archive %s: %w", dist, err)
	}
	return tw.Close()
}
