package guest

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// webDigestFile records, INSIDE the staged guest directory, the digest of the
// asset tree lever put there. Unlike the scion binary it is a marker: it
// attests what lever installed, not what is there now. The skip also requires
// layout.WebAssetsSentinel, so a wiped directory re-stages; an out-of-band
// edit of one asset in the root-owned directory goes unnoticed, at worst a
// stale UI. No security boundary rests on it.
const webDigestFile = ".lever-web-digest"

// prebuiltWebUI is webassets.Prebuilt; a variable so tests can fake the
// release.
var prebuiltWebUI = webassets.Prebuilt

// EnsureScionWebAssets builds scion's SPA on the HOST, or downloads the one
// this lever release ships for the pin, and stages it into the guest at
// layout.WebAssetsDir.
func (g Guest) EnsureScionWebAssets(ctx context.Context, spec ScionSpec) error {
	if !spec.BuildsWebAssets() {
		return nil
	}
	if spec.Version != "" {
		dist, digest, err := prebuiltWebUI(ctx, spec.Version)
		if err == nil {
			return g.stageWebAssets(ctx, dist, digest)
		}
		if !scionbin.ReleaseUnavailable(err) {
			return err
		}
		if scionbin.DownloadsWebUI(spec.Version) {
			progress.From(ctx).Note("prebuilt scion web UI not downloaded; using a source build")
		}
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

func (g Guest) stageWebAssets(ctx context.Context, dist, digest string) error {
	if g.stagedWebDigest(ctx) == digest {
		return nil
	}
	pr, pw := io.Pipe()
	tarErr := make(chan error, 1)
	go func() {
		err := webassets.WriteTar(pw, dist)
		_ = pw.CloseWithError(err)
		tarErr <- err
	}()
	err := g.pipeInto(ctx, g.RootPrefix, pr, stageWebAssetsScript(digest))
	_ = pr.Close()
	if terr := <-tarErr; terr != nil {
		return fmt.Errorf("stage scion web assets into guest at %s: pack %s: %w", layout.WebAssetsDir, dist, terr)
	}
	if err != nil {
		return fmt.Errorf("stage scion web assets into guest at %s: %w", layout.WebAssetsDir, err)
	}
	return nil
}

// stagedWebDigest returns "" when unreadable, so the guest re-stages. `test
// -f` on the sentinel catches a directory whose contents were removed but
// whose marker survived.
func (g Guest) stagedWebDigest(ctx context.Context) string {
	script := fmt.Sprintf("test -f %s && cat %s",
		shellSingleQuote(filepath.Join(layout.WebAssetsDir, filepath.FromSlash(layout.WebAssetsSentinel))),
		shellSingleQuote(filepath.Join(layout.WebAssetsDir, webDigestFile)))
	// Absolute path: a bare name resolves on the run user's PATH, which has
	// run-user-writable directories ahead of /usr/bin, and a shim there
	// could pin a replaced tree forever without guest root.
	res, err := g.UserRun(ctx, "/bin/bash", "-c", script)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// stageWebAssetsScript: the rm+mv pair leaves a brief window with no assets;
// a failure inside it leaves the destination absent rather than half-written,
// which the next apply re-stages.
//
// --no-same-owner: extraction runs as guest root, and GNU tar as root
// restores the archive's ownership by default.
//
// `bash` and `tar` are bare names because they resolve on root's PATH, which
// no run user can write to.
func stageWebAssetsScript(digest string) string {
	tmp := layout.WebAssetsDir + ".tmp"
	return fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -xf - --no-same-owner -C %s && printf %%s %s > %s && rm -rf %s && mv %s %s",
		shellSingleQuote(tmp),
		shellSingleQuote(tmp),
		shellSingleQuote(tmp),
		shellSingleQuote(digest),
		shellSingleQuote(filepath.Join(tmp, webDigestFile)),
		shellSingleQuote(layout.WebAssetsDir),
		shellSingleQuote(tmp),
		shellSingleQuote(layout.WebAssetsDir))
}
