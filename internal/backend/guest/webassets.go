package guest

import (
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
)

// ScionWebAssetsDir is where lever stages scion's built SPA inside the guest,
// and therefore the value it passes to `scion server start --web-assets-dir`.
// It sits beside scionDestPath because both answer the same question — where
// does lever put scion's parts in the guest — and both ends of the contract
// (the staging below, the flag in internal/apply) must name the SAME path.
//
// /usr/local/share is the FHS home for architecture-independent data installed
// outside the package manager, which is exactly what a built SPA is.
const ScionWebAssetsDir = "/usr/local/share/scion/web"

// webAssetsSentinel is the file scion itself treats as proof that an asset set
// is usable: cmd/server_foreground.go warns "assets are incomplete (main.js
// missing)" when it cannot stat this path, and pkg/hub/web.go's Go-rendered
// app shell loads exactly this URL. lever reuses it rather than inventing its
// own completeness signal, so "lever thinks the build is complete" and "scion
// will serve a working UI" cannot drift apart. Slash-separated: it is both a
// URL path and a path under the dist root.
const webAssetsSentinel = "assets/main.js"

// webAssetsExclude drops vite's sourcemaps from the staged payload. They are
// 71% of the build output (8.2MB of 11.5MB at pin e82a2a08, 79 files) and they
// ship to every guest on every pin, to debug a SPA lever does not develop. The
// pattern is shell-quoted at both ends of the pipeline: unquoted it would be
// globbed by the host shell before tar ever saw it.
//
// Deliberately not configurable. Nobody has asked to debug scion's minified
// bundle from inside a lever guest; a knob can be added the day someone does.
const webAssetsExclude = "*.map"

// webDigestFile records, INSIDE the staged guest directory, the digest of the
// asset tree lever put there. Unlike the scion binary — which is verified by
// hashing the installed file itself (see InstallRootBinaryIfChanged) — a tree
// of ~700 files cannot be re-hashed over the transport cheaply, and a
// host/guest hash agreement would be a brittle second implementation of the
// same walk. So this one is a marker, with the marker's honest limitation:
// it attests what lever installed, not what is there now.
//
// The limitation is bounded deliberately. The skip ALSO requires
// webAssetsSentinel to exist, so a wiped or half-wiped directory re-stages;
// what a marker misses is an out-of-band edit of an individual asset, in a
// root-owned directory inside the VM, whose worst outcome is a stale UI. No
// security boundary rests on it — that is the proxy's PAT and origin gate.
const webDigestFile = ".lever-web-digest"

// webBuildMarker is written into a host build directory as the LAST step of a
// successful build, so an interrupted `npm ci` (or a build killed mid-vite)
// leaves a directory that is visibly incomplete and gets rebuilt. It holds the
// digest it was built from, which makes a stale directory self-describing when
// someone inspects the cache by hand.
const webBuildMarker = ".lever-web-build"

// minNodeMajor mirrors scion's own web/package.json `engines.node` (">=20.0.0").
// Checked explicitly because npm only WARNS about an engines mismatch by
// default: without this, an old node produces a syntax error from deep inside
// vite instead of naming the actual problem.
const minNodeMajor = 20

// errNodeToolchain is the sentinel for "this host cannot build scion's web UI".
// A sentinel rather than a bare message so the apply path and `lever doctor`
// can share one diagnosis, and one remediation, for the same condition.
var errNodeToolchain = errors.New("node/npm toolchain not usable")

// NodeToolchainFix is the remediation printed for errNodeToolchain. It names the
// asdf/mise shim case explicitly because that is the failure this project keeps
// meeting: a shim is ON PATH and resolves, but the version it points at is not
// installed, so it exits 126 with no useful text (the same trap checkGoToolchain
// was written for).
const NodeToolchainFix = `put a REAL node+npm on PATH (not just an asdf/mise shim), e.g. export PATH="$HOME/.asdf/installs/nodejs/<ver>/bin:$PATH"; ` +
	"`node --version` should print"

// BuildsWebAssets reports whether lever both WANTS and CAN build scion's SPA
// for this spec.
//
// Binary mode is the exempt case, and deliberately not an error: with a
// prebuilt binary lever has no scion SOURCE to build the SPA from, and the
// operator who produced that binary may well have embedded the assets already
// (upstream's `make all` does). Skipping leaves those embedded assets serving;
// failing would break a working setup. config.App.ScionWebAssets is the
// user-facing form of this predicate and must agree with it — it decides both
// WebUI here and whether --web-assets-dir reaches the hub.
func (s ScionSpec) BuildsWebAssets() bool {
	return s.WebUI && s.Binary == "" && (s.Source != "" || s.Version != "")
}

// EnsureScionWebAssets builds scion's SPA on the HOST and stages it into the
// guest at ScionWebAssetsDir.
//
// It runs host-side for the same reason the scion binary is cross-compiled
// host-side: the guest carries no toolchain at all — no Go, and no node — and
// giving it one would also mean giving the jail npm's egress. Keeping the
// build out here keeps the registry traffic on the host.
//
// The work is skipped at two levels, so a re-apply on an unchanged pin costs
// two cheap probes and no npm: the host build directory is keyed by a digest
// of the scion web sources (a fetched module version is immutable, so the same
// pin always hits the same directory), and the guest staging is skipped when
// the guest already holds that same digest.
func (g Guest) EnsureScionWebAssets(ctx context.Context, spec ScionSpec) error {
	if !spec.BuildsWebAssets() {
		return nil
	}
	srcWeb, err := g.webSourceDir(ctx, spec)
	if err != nil {
		return err
	}
	dist, digest, err := g.buildWebAssets(ctx, srcWeb)
	if err != nil {
		return err
	}
	return g.stageWebAssets(ctx, dist, digest)
}

// webSourceDir resolves the host directory holding scion's npm project (the
// module's or checkout's `web/`). The Version branch reuses fetchScionModule,
// so the SPA is built from exactly the source tree the pinned binary was
// compiled from — one download, one pin, no way for the two to disagree.
//
// That is a second `go mod download` in the same apply (resolveScionBinary ran
// the first). It is deliberate: the call is idempotent and costs ~0.3s against
// a warm module cache, which is cheaper than threading the resolved directory
// out of the binary path and through two backends to get here.
func (g Guest) webSourceDir(ctx context.Context, spec ScionSpec) (string, error) {
	root := spec.Source
	if spec.Version != "" {
		_, dir, err := g.fetchScionModule(ctx, spec.Version)
		if err != nil {
			return "", err
		}
		root = dir
	}
	if root == "" {
		// Unreachable via BuildsWebAssets; a guard against a future caller
		// that skips the predicate rather than a condition users can hit.
		return "", errors.New("no scion source to build web assets from")
	}
	web := filepath.Join(root, "web")
	if _, err := os.Stat(filepath.Join(web, "package.json")); err != nil {
		return "", fmt.Errorf("scion web sources not found at %s (no package.json): %w", web, err)
	}
	return web, nil
}

// WebBuildCacheRoot is the host directory holding per-pin scion web builds.
//
// A user CACHE directory, not TempDir where the cross-compiled scion binary
// goes: that binary is one file that Go's build cache reproduces in seconds,
// whereas this holds a ~280-package node_modules per pin and losing it to a
// tmp sweep costs a full re-download. Exported so `lever doctor` can probe the
// node toolchain from the SAME directory the build will run in — see
// CheckNodeToolchain.
func WebBuildCacheRoot() (string, error) {
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
// nothing about the build, which runs under WebBuildCacheRoot. Both `lever
// apply` and `lever doctor` therefore probe in the build's own directory, and
// get the same answer for the same reason.
func CheckNodeToolchain(ctx context.Context, r exec.Runner, probeDir string) (string, error) {
	res, err := r.RunIn(ctx, probeDir, nil, "node", "--version")
	if err != nil {
		return "", fmt.Errorf("%w: node --version: %v", errNodeToolchain, err)
	}
	version := strings.TrimSpace(res.Stdout)
	major, err := nodeMajor(version)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNodeToolchain, err)
	}
	if major < minNodeMajor {
		return "", fmt.Errorf("%w: node %s is too old — scion's web UI needs node >= %d (its package.json engines)",
			errNodeToolchain, version, minNodeMajor)
	}
	if _, err := r.RunIn(ctx, probeDir, nil, "npm", "--version"); err != nil {
		return "", fmt.Errorf("%w: npm --version: %v", errNodeToolchain, err)
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

// buildWebAssets produces a built dist/client on the host and returns its path
// plus the digest identifying it. It builds only when the cache does not
// already hold a COMPLETE build for that digest.
func (g Guest) buildWebAssets(ctx context.Context, srcWeb string) (dist, digest string, err error) {
	digest, err = hashWebSource(srcWeb)
	if err != nil {
		return "", "", fmt.Errorf("hashing scion web sources at %s: %w", srcWeb, err)
	}
	root, err := WebBuildCacheRoot()
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
	if _, err := CheckNodeToolchain(ctx, g.Host, root); err != nil {
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
	if _, err := g.Host.RunIn(ctx, staging, nil, "npm", "ci", "--no-audit", "--no-fund"); err != nil {
		return "", "", fmt.Errorf("npm ci for scion web assets in %s: %w", staging, err)
	}
	if _, err := g.Host.RunIn(ctx, staging, nil, "npm", "run", "build"); err != nil {
		return "", "", fmt.Errorf("npm run build for scion web assets in %s: %w", staging, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "dist", "client", filepath.FromSlash(webAssetsSentinel))); err != nil {
		return "", "", fmt.Errorf("scion web build produced no %s — the build reported success but its output is unusable", webAssetsSentinel)
	}
	// Marker before the rename, so what lands at buildDir is complete the
	// instant it is visible there. Nothing may observe a half-built cache.
	if err := os.WriteFile(filepath.Join(staging, webBuildMarker), []byte(digest), 0o644); err != nil {
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
	if _, err := os.Stat(filepath.Join(buildDir, webBuildMarker)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(buildDir, "dist", "client", filepath.FromSlash(webAssetsSentinel)))
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

// hashWebSource digests the build INPUTS under root: every regular file's
// relative path and contents, walked in the deterministic order WalkDir
// guarantees (lexical).
//
// Contents, not the pin string. For a `version:` pin the two are equivalent —
// the module cache is immutable, so identical sources mean an identical pin —
// but a `source:` checkout is edited in place, and keying on contents makes
// that case correct for free instead of silently serving a stale UI. Paths are
// hashed alongside contents so a rename is a change; the NUL separators keep
// path and content boundaries unambiguous.
func hashWebSource(root string) (string, error) {
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

// stageWebAssets streams the built dist/client into the guest, unless the guest
// already holds this digest.
func (g Guest) stageWebAssets(ctx context.Context, dist, digest string) error {
	if g.stagedWebDigest(ctx) == digest {
		return nil
	}
	if _, err := g.Host.Run(ctx, nil, "bash", "-c", stageWebAssetsScript(g.RootPrefix, dist, digest)); err != nil {
		return fmt.Errorf("stage scion web assets into guest at %s: %w", ScionWebAssetsDir, err)
	}
	return nil
}

// stagedWebDigest reads the digest the guest currently holds, or "" when there
// is none. `test -f` on the sentinel guards the marker's blind spot: a
// directory whose contents were removed but whose marker survived would
// otherwise be accepted forever. Fail-closed by returning "" — an unreadable
// guest re-stages, which costs 12MB of transport rather than an unusable UI.
func (g Guest) stagedWebDigest(ctx context.Context) string {
	script := fmt.Sprintf("test -f %s && cat %s",
		shellSingleQuote(filepath.Join(ScionWebAssetsDir, filepath.FromSlash(webAssetsSentinel))),
		shellSingleQuote(filepath.Join(ScionWebAssetsDir, webDigestFile)))
	// Absolute path, not a bare name: userRun passes no env, so a bare name
	// resolves on the guest run-user's PATH, which precedes /usr/bin with
	// run-user-writable directories (the same reasoning as
	// InstallRootBinaryIfChanged's /usr/bin/sha256sum). A shim planted there
	// could echo the current digest for a tree it had replaced and pin the
	// staged UI stale forever, with no guest root needed.
	res, err := g.userRun(ctx, "/bin/bash", "-c", script)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// stageWebAssetsScript builds the host shell pipeline that streams the asset
// tree into the guest. Split out from stageWebAssets so a test can assert the
// argv without a guest — the same shape internal/backend's InstallGuestBinary
// argv tests use.
//
// It mirrors InstallRootBinary's transport (the Runner has no stdin channel, so
// the host `tar -cf -` is piped into a guest-side `bash -c`) and its atomicity:
// extract into a temp directory, then swap. A directory swap cannot be a single
// atomic mv the way a file replace can, so the rm+mv pair leaves a brief window
// with no assets; a failure inside it leaves the destination absent rather than
// half-written, which the next apply re-stages because stagedWebDigest reads "".
//
// Three tar details are load-bearing:
//   - COPYFILE_DISABLE=1 stops macOS bsdtar turning extended attributes into
//     AppleDouble `._name` members. vite's output carries xattrs, so without
//     this the guest gets a junk `._file` beside every real asset.
//   - --no-same-owner: extraction runs as guest root, and GNU tar as root
//     restores the ARCHIVE's ownership by default — the host's uid, which
//     need not exist in the guest.
//   - `set -o pipefail` so a host-side tar failure is not masked by a
//     successful guest-side one. bash, not sh, because dash lacks pipefail.
//
// The guest-side `bash` and `tar` stay bare names, unlike the run-user probe in
// stagedWebDigest: they resolve on guest ROOT's PATH, which no run-user can
// write to, so planting a shim there already requires the root this command
// runs as. That also matches InstallRootBinary's existing convention.
//
// Paths are shell-quoted for the same reason InstallRootBinary quotes its
// destPath: the guest script is embedded as an argument inside the host script,
// so an unquoted path containing a single quote would break out of the inner
// quoting and run as a host command.
func stageWebAssetsScript(rootPrefix []string, dist, digest string) string {
	rootWords := make([]string, 0, len(rootPrefix))
	for _, w := range rootPrefix {
		rootWords = append(rootWords, shellSingleQuote(w))
	}
	tmp := ScionWebAssetsDir + ".tmp"
	inner := fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -xf - --no-same-owner -C %s && printf %%s %s > %s && rm -rf %s && mv %s %s",
		shellSingleQuote(tmp),
		shellSingleQuote(tmp),
		shellSingleQuote(tmp),
		shellSingleQuote(digest),
		shellSingleQuote(filepath.Join(tmp, webDigestFile)),
		shellSingleQuote(ScionWebAssetsDir),
		shellSingleQuote(tmp),
		shellSingleQuote(ScionWebAssetsDir))
	return fmt.Sprintf("set -o pipefail; COPYFILE_DISABLE=1 tar -cf - --exclude=%s -C %s . | %s bash -c %s",
		shellSingleQuote(webAssetsExclude), shellSingleQuote(dist), strings.Join(rootWords, " "), shellSingleQuote(inner))
}
