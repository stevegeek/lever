// Package webassets builds scion's web UI (the vite SPA under scion's web/)
// on the HOST and returns a finished dist/client directory, cached per source
// digest. It runs host-side for the same reason the scion binary is
// cross-compiled host-side: the guest carries no toolchain at all — no Go, and
// no node — and giving it one would also mean giving the jail npm's egress.
// Keeping the build out here keeps the registry traffic on the host.
//
// Staging the result into a guest is internal/backend/guest's job; this
// package only knows node, npm and the cache directory.
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

	"github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// webAssetsExclude drops vite's sourcemaps from the staged payload. They are
// 71% of the build output (8.2MB of 11.5MB at pin e82a2a08, 79 files) and they
// ship to every guest on every pin, to debug a SPA lever does not develop. A
// filepath.Match pattern against each file's base name (see WriteTar).
//
// Deliberately not configurable. Nobody has asked to debug scion's minified
// bundle from inside a lever guest; a knob can be added the day someone does.
const webAssetsExclude = "*.map"

// BuildMarker is written into a host build directory as the LAST step of a
// successful build, so an interrupted `npm ci` (or a build killed mid-vite)
// leaves a directory that is visibly incomplete and gets rebuilt. It holds the
// digest it was built from, which makes a stale directory self-describing when
// someone inspects the cache by hand.
const BuildMarker = ".lever-web-build"

// minNodeMajor mirrors scion's own web/package.json `engines.node` (">=20.0.0").
// Checked explicitly because npm only WARNS about an engines mismatch by
// default: without this, an old node produces a syntax error from deep inside
// vite instead of naming the actual problem.
const minNodeMajor = 20

// ErrNodeToolchain is the sentinel for "this host cannot build scion's web UI".
// A sentinel rather than a bare message so the apply path and `lever doctor`
// (errors.Is) can share one diagnosis, and one remediation, for the same
// condition.
var ErrNodeToolchain = errors.New("node/npm toolchain not usable")

// NodeToolchainFix is the remediation printed for ErrNodeToolchain. It names the
// asdf/mise shim case explicitly because that is the failure this project keeps
// meeting: a shim is ON PATH and resolves, but the version it points at is not
// installed, so it exits 126 with no useful text (the same trap checkGoToolchain
// was written for).
const NodeToolchainFix = `put a REAL node+npm on PATH (not just an asdf/mise shim), e.g. export PATH="$HOME/.asdf/installs/nodejs/<ver>/bin:$PATH"; ` +
	"`node --version` should print"

// SourceDir resolves the host directory holding scion's npm project (the
// module's or checkout's `web/`). The Version branch reuses scionbin.FetchModule,
// so the SPA is built from exactly the source tree the pinned binary was
// compiled from — one download, one pin, no way for the two to disagree.
//
// That is a second `go mod download` in the same apply (scionbin.Resolve ran
// the first). It is deliberate: the call is idempotent and costs ~0.3s against
// a warm module cache, which is cheaper than threading the resolved directory
// out of the binary path and through two backends to get here.
func SourceDir(ctx context.Context, r exec.Runner, spec scionbin.Spec) (string, error) {
	root := spec.Source
	if spec.Version != "" {
		_, dir, err := scionbin.FetchModule(ctx, r, spec.Version)
		if err != nil {
			return "", err
		}
		root = dir
	}
	if root == "" {
		// Unreachable via Spec.BuildsWebAssets; a guard against a future caller
		// that skips the predicate rather than a condition users can hit.
		return "", errors.New("no scion source to build web assets from")
	}
	web := filepath.Join(root, "web")
	if _, err := os.Stat(filepath.Join(web, "package.json")); err != nil {
		return "", fmt.Errorf("scion web sources not found at %s (no package.json): %w", web, err)
	}
	return web, nil
}

// CacheRoot is the host directory holding per-pin scion web builds.
//
// A user CACHE directory, not TempDir where the cross-compiled scion binary
// goes: that binary is one file that Go's build cache reproduces in seconds,
// whereas this holds a ~280-package node_modules per pin and losing it to a
// tmp sweep costs a full re-download. Exported so `lever doctor` can probe the
// node toolchain from the SAME directory the build will run in — see
// CheckNodeToolchain.
func CacheRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(cache, "lever", "scion-web"), nil
}

// CheckNodeToolchain verifies a real, working node+npm is resolvable for the
// web build, and returns the node version it found.
//
// probeDir is why this is a function and not two inline `node --version` calls.
// A version manager that resolves node by walking UP from the working directory
// (asdf, mise) can answer differently in different directories, so a probe run
// in the user's project — which may have its own .tool-versions — proves
// nothing about the build, which runs under CacheRoot. Both `lever
// apply` and `lever doctor` therefore probe in the build's own directory, and
// get the same answer for the same reason.
func CheckNodeToolchain(ctx context.Context, r exec.Runner, probeDir string) (string, error) {
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

// nodeMajor parses the major version out of `node --version` output ("v25.9.0").
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

// Build produces a built dist/client on the host and returns its path plus
// the digest identifying it. It builds only when the cache does not already
// hold a COMPLETE build for that digest.
//
// The host build directory is keyed by a digest of the scion web sources (a
// fetched module version is immutable, so the same pin always hits the same
// directory), so a re-apply on an unchanged pin costs one cheap probe and no
// npm.
func Build(ctx context.Context, r exec.Runner, srcWeb string) (dist, digest string, err error) {
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
	// Probe BEFORE anything is written: a missing toolchain is the common
	// first-run failure, and there is no reason to copy a few thousand files
	// only to then say node is unusable. The probe runs under root rather than
	// the build directory (which does not exist yet) — same directory
	// ancestry, so a walk-up version manager resolves identically.
	if _, err := CheckNodeToolchain(ctx, r, root); err != nil {
		return "", "", fmt.Errorf("%w\n    fix: %s", err, NodeToolchainFix)
	}
	// Build in a private directory and rename it into place, rather than
	// building at buildDir directly. Two reasons, both real:
	//
	//   - Concurrency. Two remote-enabled instances on one host share this
	//     cache, and applying them at once would otherwise run two npm builds
	//     in the same directory — with the loser's RemoveAll deleting the
	//     winner's node_modules mid-install. A rename is atomic, so the worst
	//     case becomes wasted work, never corruption.
	//   - Half-finished state. npm's copy:client writes build output back into
	//     public/, which vite then copies forward into the NEXT dist, so a
	//     reused directory can accumulate assets from an older build. Starting
	//     empty every time removes that entirely.
	staging, err := os.MkdirTemp(root, ".build-")
	if err != nil {
		return "", "", fmt.Errorf("create web build staging dir under %s: %w", root, err)
	}
	// After a successful rename this path is gone and RemoveAll is a no-op; on
	// every failure path it is what stops a dead build from accumulating.
	defer func() { _ = os.RemoveAll(staging) }()

	// The Go module cache is read-only (0444/0555), so the npm project has to
	// be copied somewhere writable before npm can touch it.
	if err := copyTree(srcWeb, staging, skipWebSourcePath); err != nil {
		return "", "", fmt.Errorf("stage scion web sources for build: %w", err)
	}
	// `npm ci`, not `npm install`: it installs exactly package-lock.json and
	// fails if the lock and manifest disagree, which is what makes a given pin
	// build the same way twice. --no-audit/--no-fund drop two registry
	// round-trips that only produce console noise.
	if _, err := r.RunIn(ctx, staging, nil, "npm", "ci", "--no-audit", "--no-fund"); err != nil {
		return "", "", fmt.Errorf("npm ci for scion web assets in %s: %w", staging, err)
	}
	if _, err := r.RunIn(ctx, staging, nil, "npm", "run", "build"); err != nil {
		return "", "", fmt.Errorf("npm run build for scion web assets in %s: %w", staging, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "dist", "client", filepath.FromSlash(layout.WebAssetsSentinel))); err != nil {
		return "", "", fmt.Errorf("scion web build produced no %s — the build reported success but its output is unusable", layout.WebAssetsSentinel)
	}
	// Marker before the rename, so what lands at buildDir is complete the
	// instant it is visible there. Nothing may observe a half-built cache.
	if err := os.WriteFile(filepath.Join(staging, BuildMarker), []byte(digest), 0o644); err != nil {
		return "", "", fmt.Errorf("marking scion web build complete: %w", err)
	}
	if err := os.Rename(staging, buildDir); err != nil {
		// Someone finished the same build first. Their output is identical by
		// construction — same digest, same sources — so adopt it.
		if webBuildComplete(buildDir) {
			return dist, digest, nil
		}
		// Otherwise the destination is occupied by something incomplete: a
		// hand-edited cache, or a build interrupted by an older lever that
		// built in place. Clear it and retry once.
		if rmErr := os.RemoveAll(buildDir); rmErr != nil {
			return "", "", fmt.Errorf("clear incomplete web build %s: %w", buildDir, rmErr)
		}
		if err := os.Rename(staging, buildDir); err != nil {
			return "", "", fmt.Errorf("publish web build to %s: %w", buildDir, err)
		}
	}
	return dist, digest, nil
}

// webBuildComplete reports whether buildDir already holds a finished build:
// the completion marker AND the asset scion actually serves. Both, because the
// marker alone would survive someone clearing dist/, and the sentinel alone
// would accept a build interrupted after vite but before the rest.
func webBuildComplete(buildDir string) bool {
	if _, err := os.Stat(filepath.Join(buildDir, BuildMarker)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(buildDir, "dist", "client", filepath.FromSlash(layout.WebAssetsSentinel)))
	return err == nil
}

// skipWebSourcePath reports whether a path relative to scion's web/ is build
// OUTPUT rather than build INPUT, and so must be excluded from both the digest
// and the copy.
//
// node_modules and dist are the obvious ones. public/assets and public/shoelace
// are the subtle ones: npm's postinstall and copy:client scripts GENERATE them
// inside the source tree, so hashing them would make a source checkout's digest
// change every time it was built — a cache that never hits, and a rebuild on
// every apply.
func skipWebSourcePath(rel string) bool {
	switch rel {
	case "node_modules", "dist", "public/assets", "public/shoelace":
		return true
	}
	return false
}

// HashSource digests the build INPUTS under root: every regular file's
// relative path and contents, walked in the deterministic order WalkDir
// guarantees (lexical).
//
// Contents, not the pin string. For a `version:` pin the two are equivalent —
// the module cache is immutable, so identical sources mean an identical pin —
// but a `source:` checkout is edited in place, and keying on contents makes
// that case correct for free instead of silently serving a stale UI. Paths are
// hashed alongside contents so a rename is a change; the NUL separators keep
// path and content boundaries unambiguous.
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

// copyTree copies the regular files under src into dst, skipping any relative
// path skip reports. Modes are NORMALISED (0755 dirs, 0644 files) rather than
// preserved: the source is usually Go's module cache, where everything is
// read-only, and npm must be able to write into its own project directory.
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

// WriteTar writes dist as a tar archive to w: regular files and
// symlinks, paths relative to dist, sourcemaps (webAssetsExclude) dropped.
// Written by lever rather than the host's tar so the archive is the same from
// every host: no AppleDouble `._name` members from macOS bsdtar turning
// xattrs into files, no host uid/gid to restore (the entries carry none — the
// extract side runs as guest root and --no-same-owner makes it root's anyway).
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
