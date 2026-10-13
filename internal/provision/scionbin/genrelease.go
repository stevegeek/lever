package scionbin

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision"
)

// GenerateOptions configure GenerateRelease.
type GenerateOptions struct {
	// LeverVersion is the release being built, without its "v".
	LeverVersion string
	// Pin is the scion version to build (ReleasePin).
	Pin string
	// AssetDir receives the gzip-compressed binaries the release uploads.
	AssetDir string
	// ManifestDir receives the manifest the lever build embeds (ReleaseDir
	// of this package).
	ManifestDir string
}

// GenerateRelease builds scion at opts.Pin for every ReleaseArches entry,
// with Resolve's build flags plus CGO_ENABLED=0, writes the compressed binaries into
// AssetDir and the manifest pinning their digests into ManifestDir. The
// release build runs it before building lever, so the lever it ships embeds
// the digests of the assets it ships beside.
func GenerateRelease(ctx context.Context, r proc.Runner, opts GenerateOptions) error {
	manifestPath := filepath.Join(opts.ManifestDir, ReleaseManifestFile)
	// Removed first: a generation that stops half way must not leave a
	// manifest naming assets it did not write.
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	goBin, err := provision.GoBinary(ctx, r)
	if err != nil {
		return err
	}
	res, err := r.Run(ctx, nil, goBin, "mod", "download", "-json", ModulePath+"@"+opts.Pin)
	if err != nil {
		return fmt.Errorf("download scion %s: %w", opts.Pin, err)
	}
	var dl struct {
		Version, Dir, Error string
		Origin              struct{ Hash string }
	}
	if err := json.Unmarshal([]byte(res.Stdout), &dl); err != nil {
		return fmt.Errorf("parse `go mod download` output for scion %s: %w", opts.Pin, err)
	}
	if dl.Error != "" || dl.Dir == "" {
		return fmt.Errorf("download scion %s: %q", opts.Pin, dl.Error)
	}
	rel := Release{
		LeverVersion:  opts.LeverVersion,
		ScionVersion:  opts.Pin,
		ModuleVersion: dl.Version,
		Commit:        dl.Origin.Hash,
		Assets:        map[string]Asset{},
	}
	if err := os.MkdirAll(opts.AssetDir, 0o755); err != nil {
		return err
	}
	// The release uploads every asset in the directory, so one left from
	// another pin or run must not stay beside the new ones.
	stale, err := filepath.Glob(filepath.Join(opts.AssetDir, "scion_*_linux_*.gz"))
	if err != nil {
		return err
	}
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	stage, err := os.MkdirTemp("", "lever-scion-release-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for _, arch := range ReleaseArches {
		bin := filepath.Join(stage, "scion-"+arch)
		// Static: scion's SQLite driver is pure Go, and a binary that needs
		// no libc runs on any guest release (upstream builds it so too).
		if _, err := r.RunIn(ctx, dl.Dir, map[string]string{"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"},
			goBin, buildArgs(bin)...); err != nil {
			return fmt.Errorf("build scion for linux/%s: %w", arch, err)
		}
		if err := VerifyELFArch(bin, arch); err != nil {
			return err
		}
		a, err := compressAsset(bin, opts.AssetDir, "scion_"+opts.Pin+"_linux_"+arch+".gz")
		if err != nil {
			return fmt.Errorf("linux/%s: %w", arch, err)
		}
		rel.Assets[arch] = a
	}
	if err := rel.validate(); err != nil {
		return fmt.Errorf("generated manifest: %w", err)
	}
	b, err := json.MarshalIndent(rel, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.ManifestDir, 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(manifestPath, append(b, '\n'), 0o644)
}

// compressAsset gzips bin into dir/name and returns both digests and sizes.
// gzip's header carries no name or time here, so the same binary always
// gives the same asset.
func compressAsset(bin, dir, name string) (Asset, error) {
	a := Asset{Name: name}
	in, err := os.Open(bin)
	if err != nil {
		return a, err
	}
	defer func() { _ = in.Close() }()
	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		return a, err
	}
	defer func() { _ = out.Close() }()
	binHash, gzHash := sha256.New(), sha256.New()
	zw, err := gzip.NewWriterLevel(io.MultiWriter(out, gzHash), gzip.BestCompression)
	if err != nil {
		return a, err
	}
	if a.BinarySize, err = io.Copy(io.MultiWriter(zw, binHash), in); err != nil {
		return a, err
	}
	if err := zw.Close(); err != nil {
		return a, err
	}
	if err := out.Close(); err != nil {
		return a, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return a, err
	}
	a.Size = fi.Size()
	a.SHA256 = hex.EncodeToString(gzHash.Sum(nil))
	a.BinarySHA256 = hex.EncodeToString(binHash.Sum(nil))
	return a, nil
}
