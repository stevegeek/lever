package guest

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/scion/layout"
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

// webDigestFile records, INSIDE the staged guest directory, the digest of the
// asset tree lever put there. Unlike the scion binary — which is verified by
// hashing the installed file itself (see InstallRootBinaryIfChanged) — a tree
// of ~700 files cannot be re-hashed over the transport cheaply, and a
// host/guest hash agreement would be a brittle second implementation of the
// same walk. So this one is a marker, with the marker's honest limitation:
// it attests what lever installed, not what is there now.
//
// The limitation is bounded deliberately. The skip ALSO requires
// layout.WebAssetsSentinel to exist, so a wiped or half-wiped directory
// re-stages; what a marker misses is an out-of-band edit of an individual
// asset, in a root-owned directory inside the VM, whose worst outcome is a
// stale UI. No security boundary rests on it — that is the proxy's PAT and
// origin gate.
const webDigestFile = ".lever-web-digest"

// EnsureScionWebAssets builds scion's SPA on the HOST (webassets.Build) and
// stages it into the guest at ScionWebAssetsDir.
//
// The work is skipped at two levels, so a re-apply on an unchanged pin costs
// two cheap probes and no npm: the host build cache is keyed by a digest of
// the scion web sources, and the guest staging is skipped when the guest
// already holds that same digest.
func (g Guest) EnsureScionWebAssets(ctx context.Context, spec ScionSpec) error {
	if !spec.BuildsWebAssets() {
		return nil
	}
	srcWeb, err := webassets.SourceDir(ctx, g.Host, spec)
	if err != nil {
		return err
	}
	dist, digest, err := webassets.Build(ctx, g.Host, srcWeb)
	if err != nil {
		return err
	}
	return g.stageWebAssets(ctx, dist, digest)
}

// stageWebAssets streams the built dist/client into the guest, unless the guest
// already holds this digest. The tree travels as a tar archive written by
// lever itself (webassets.WriteTar) on the stdin of a guest-side extract
// script — argv only, through the Runner's stdin seam, like InstallRootBinary.
func (g Guest) stageWebAssets(ctx context.Context, dist, digest string) error {
	if g.stagedWebDigest(ctx) == digest {
		return nil
	}
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(webassets.WriteTar(pw, dist)) }()
	err := g.pipeInto(ctx, g.RootPrefix, pr, stageWebAssetsScript(digest))
	_ = pr.Close()
	if err != nil {
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
		shellSingleQuote(filepath.Join(ScionWebAssetsDir, filepath.FromSlash(layout.WebAssetsSentinel))),
		shellSingleQuote(filepath.Join(ScionWebAssetsDir, webDigestFile)))
	// Absolute path, not a bare name: UserRun passes no env, so a bare name
	// resolves on the guest run-user's PATH, which precedes /usr/bin with
	// run-user-writable directories (the same reasoning as
	// InstallRootBinaryIfChanged's /usr/bin/sha256sum). A shim planted there
	// could echo the current digest for a tree it had replaced and pin the
	// staged UI stale forever, with no guest root needed.
	res, err := g.UserRun(ctx, "/bin/bash", "-c", script)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// stageWebAssetsScript is the guest-side half of stageWebAssets: extract the
// archive on stdin into a temp directory, record the digest inside it, then
// swap it in. Split out so a test can assert it without a guest.
//
// A directory swap cannot be a single atomic mv the way a file replace can, so
// the rm+mv pair leaves a brief window with no assets; a failure inside it
// leaves the destination absent rather than half-written, which the next apply
// re-stages because stagedWebDigest reads "".
//
// --no-same-owner: extraction runs as guest root, and GNU tar as root restores
// the ARCHIVE's ownership by default; lever's archive carries none, and the
// flag makes that explicit.
//
// `bash` and `tar` stay bare names, unlike the run-user probe in
// stagedWebDigest: they resolve on guest ROOT's PATH, which no run-user can
// write to, so planting a shim there already requires the root this command
// runs as. Paths and the digest are shell-quoted because they are interpolated
// into the script.
func stageWebAssetsScript(digest string) string {
	tmp := ScionWebAssetsDir + ".tmp"
	return fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -xf - --no-same-owner -C %s && printf %%s %s > %s && rm -rf %s && mv %s %s",
		shellSingleQuote(tmp),
		shellSingleQuote(tmp),
		shellSingleQuote(tmp),
		shellSingleQuote(digest),
		shellSingleQuote(filepath.Join(tmp, webDigestFile)),
		shellSingleQuote(ScionWebAssetsDir),
		shellSingleQuote(tmp),
		shellSingleQuote(ScionWebAssetsDir))
}
