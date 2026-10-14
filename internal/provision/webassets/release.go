package webassets

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// Limits on what a release web archive may unpack to. The archive's digest
// is pinned in lever, so these only matter if the release itself is bad;
// they keep that from filling the host's disk. A built UI is a few hundred
// files and a few MB. Variables so tests can lower them.
var (
	maxReleaseEntries         = 10000
	maxReleaseFileBytes int64 = 32 << 20
	maxReleaseTreeBytes int64 = 256 << 20
)

// releaseDirPrefix names cache entries unpacked from a release archive.
// Source builds are named by a bare hex digest, so the two never collide.
const releaseDirPrefix = "release-"

// ErrBadReleaseArchive is a web archive whose digest matched but whose
// content breaks the unpack rules. It stops the apply like a digest
// mismatch: the pinned release is unusable, and building instead would hide
// that.
var ErrBadReleaseArchive = errors.New("the release's prebuilt scion web UI is not a plain tree of files")

// Prebuilt puts the web UI this lever release ships for version in the cache
// and returns its dist/client and digest, as Build does. An error for which
// scionbin.ReleaseUnavailable is true means: build from source instead.
func Prebuilt(ctx context.Context, version string) (dist, digest string, err error) {
	a, err := scionbin.ReleaseWebUI(version)
	if err != nil {
		return "", "", err
	}
	root, err := CacheRoot()
	if err != nil {
		return "", "", err
	}
	digest = releaseDirPrefix + a.SHA256
	buildDir := filepath.Join(root, digest)
	dist = filepath.Join(buildDir, "dist", "client")
	// Unlike the scion binary, a cached tree is not hashed again: it lives in
	// the operator's own cache like a source build, and is served to the
	// guest, not run as root in it.
	if webBuildComplete(buildDir) {
		progress.From(ctx).Note("prebuilt scion web UI, cached")
		return dist, digest, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", fmt.Errorf("create web build cache %s: %w", root, err)
	}
	staging, err := os.MkdirTemp(root, ".release-")
	if err != nil {
		return "", "", fmt.Errorf("create web staging dir under %s: %w", root, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	progress.From(ctx).Detail("downloading the prebuilt scion web UI")
	archive, err := os.CreateTemp(staging, "archive-*")
	if err != nil {
		return "", "", fmt.Errorf("scion web UI download: %w", err)
	}
	defer func() { _ = archive.Close() }()
	if err := scionbin.FetchWebUI(ctx, version, archive); err != nil {
		if errors.Is(err, scionbin.ErrReleaseTampered) {
			err = fmt.Errorf("%w (set %s=off to build scion and its web UI from source instead)", err, scionbin.DownloadEnv)
		}
		return "", "", err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", "", fmt.Errorf("scion web UI download: %w", err)
	}
	if err := unpackRelease(archive, filepath.Join(staging, "dist", "client")); err != nil {
		return "", "", fmt.Errorf("%s: %w", a.Name, err)
	}
	if err := archive.Close(); err != nil {
		return "", "", err
	}
	if err := os.Remove(archive.Name()); err != nil {
		return "", "", err
	}
	if err := publishBuild(staging, buildDir, digest); err != nil {
		return "", "", err
	}
	progress.From(ctx).Note("prebuilt scion web UI, downloaded")
	return dist, digest, nil
}

// unpackRelease extracts a gzip'd tar into dst, which must not exist yet.
// Only directories and regular files are accepted, every name must be a
// plain relative path, and modes are lever's (0755/0644), not the
// archive's. Nothing is followed or overwritten: dst is new and holds no
// links, and files are created exclusively, so a duplicate name fails.
func unpackRelease(r io.Reader, dst string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadReleaseArchive, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadReleaseArchive, fmt.Sprintf(format, args...))
	}
	tr := tar.NewReader(zr)
	var entries int
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return bad("%v", err)
		}
		if entries++; entries > maxReleaseEntries {
			return bad("more than %d entries", maxReleaseEntries)
		}
		name := hdr.Name
		if hdr.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
			return bad("entry %q is not a plain relative path", hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return bad("%s: %v", name, err)
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > maxReleaseFileBytes {
				return bad("%s is %d bytes, over %d", name, hdr.Size, maxReleaseFileBytes)
			}
			if total += hdr.Size; total > maxReleaseTreeBytes {
				return bad("unpacks to over %d bytes", maxReleaseTreeBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return bad("%s: %v", name, err)
			}
			if err := writeNewFile(target, tr, hdr.Size); err != nil {
				if errors.Is(err, fs.ErrExist) || errors.Is(err, io.ErrUnexpectedEOF) {
					return bad("%s: %v", name, err)
				}
				return err
			}
		default:
			return bad("entry %q has type %q; only directories and regular files are allowed", hdr.Name, hdr.Typeflag)
		}
	}
	if entries == 0 {
		return bad("empty archive")
	}
	if _, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(layout.WebAssetsSentinel))); err != nil {
		return bad("no %s", layout.WebAssetsSentinel)
	}
	return nil
}

func writeNewFile(p string, r io.Reader, size int64) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// GenerateRelease builds the web UI of the scion module source at
// moduleDir, the same way an apply does (Build), and writes it into
// assetDir as the release asset for pin.
//
// The archive is deterministic for a given build output: sorted names, no
// owners, a fixed time and lever's modes. It is unpacked with the rules
// Prebuilt applies before it is returned, so a release cannot ship an
// archive lever would refuse.
func GenerateRelease(ctx context.Context, r proc.Runner, moduleDir, assetDir, pin string) (*scionbin.WebAsset, error) {
	web := filepath.Join(moduleDir, "web")
	if _, err := os.Stat(filepath.Join(web, "package.json")); err != nil {
		return nil, fmt.Errorf("scion web sources not found at %s: %w", web, err)
	}
	dist, _, err := Build(ctx, r, web)
	if err != nil {
		return nil, err
	}
	if err := clearReleaseAssets(assetDir); err != nil {
		return nil, err
	}
	p := filepath.Join(assetDir, scionbin.WebAssetName(pin))
	if _, _, err := writeReleaseArchive(p, dist); err != nil {
		return nil, err
	}
	return checkReleaseAsset(p)
}

// ImportRelease takes the web UI archive for pin that GenerateRelease wrote
// into srcDir, in another job of the release build, as this release's asset:
// it copies it into assetDir and pins what it holds. It is
// scionbin.GenerateOptions.WebUI for the release job, which runs no npm.
func ImportRelease(srcDir, assetDir, pin string) (*scionbin.WebAsset, error) {
	name := scionbin.WebAssetName(pin)
	b, err := os.ReadFile(filepath.Join(srcDir, name))
	if err != nil {
		return nil, fmt.Errorf("prebuilt web UI: %w", err)
	}
	if err := clearReleaseAssets(assetDir); err != nil {
		return nil, err
	}
	p := filepath.Join(assetDir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return nil, err
	}
	return checkReleaseAsset(p)
}

// clearReleaseAssets removes every web asset from assetDir: the release
// uploads all of them.
func clearReleaseAssets(assetDir string) error {
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(assetDir, scionbin.WebAssetName("*")))
	if err != nil {
		return err
	}
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	return nil
}

// checkReleaseAsset unpacks the archive at p with Prebuilt's rules and
// returns its manifest entry.
func checkReleaseAsset(p string) (*scionbin.WebAsset, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	check, err := os.MkdirTemp("", "lever-web-release-check-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(check) }()
	a := &scionbin.WebAsset{Name: filepath.Base(p), SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}
	if err := unpackRelease(f, filepath.Join(check, "client")); err != nil {
		return nil, fmt.Errorf("release web UI %s: %w", a.Name, err)
	}
	return a, nil
}

// writeReleaseArchive writes dist as a gzip'd tar to p and returns its sha256
// and size.
func writeReleaseArchive(p, dist string) (string, int64, error) {
	out, err := os.Create(p)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = out.Close() }()
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(out, h)}
	zw, err := gzip.NewWriterLevel(cw, gzip.BestCompression)
	if err != nil {
		return "", 0, err
	}
	if err := writeReleaseTar(zw, dist); err != nil {
		return "", 0, err
	}
	if err := zw.Close(); err != nil {
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// writeReleaseTar is WriteTar for the release asset: directories and regular
// files only (a link would be refused on unpack, so it fails here), with
// nothing that varies by build host or time.
func writeReleaseTar(w io.Writer, dist string) error {
	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0)
	err := filepath.WalkDir(dist, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dist, path)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: epoch})
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file or directory", path)
		}
		if skip, _ := filepath.Match(webAssetsExclude, d.Name()); skip {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: info.Size(), ModTime: epoch}); err != nil {
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
