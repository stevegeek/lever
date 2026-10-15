package scionbin

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/progress"
)

// ReleasePin is the scion commit a lever release builds its prebuilt scion
// binaries from, and the scion.version the examples and docs pin.
const ReleasePin = "65531d7f"

// ReleaseDir is the directory, relative to this package, that the release
// build's generator (GenerateRelease) writes the manifest into and the embed
// reads it from.
const ReleaseDir = "release"

// ReleaseManifestFile is the manifest's name in ReleaseDir.
const ReleaseManifestFile = "manifest.json"

// ReleaseArches are the guest architectures a release ships scion for.
var ReleaseArches = []string{"amd64", "arm64"}

// DownloadEnv set to "off" (or 0, false, no) makes lever build the release
// pin from source instead of downloading the release's prebuilt binary.
const DownloadEnv = "LEVER_SCION_DOWNLOAD"

// The manifest is embedded rather than fetched beside the binaries: the
// digests that decide whether a downloaded scion may run in the jail must
// come from the lever binary the operator already trusts, never from the
// server that serves the download. A lever built without the release
// generator embeds only the placeholder and never downloads.
//
//go:embed all:release
var releaseFS embed.FS

// Release is the embedded manifest: the lever release whose assets hold the
// prebuilt scion, the scion it was built from, and each asset's pinned
// digests.
type Release struct {
	// LeverVersion is the release tag without its "v": the assets are
	// downloaded from that release.
	LeverVersion string `json:"lever_version"`
	// ScionVersion is the pin as configured (ReleasePin).
	ScionVersion string `json:"scion_version"`
	// ModuleVersion is the module version the pin resolved to.
	ModuleVersion string `json:"module_version"`
	// Commit is the full scion commit hash.
	Commit string           `json:"commit"`
	Assets map[string]Asset `json:"assets"`
	// Web is scion's web UI built from the same commit, for remote access.
	// A manifest without it still serves the binaries.
	Web *WebAsset `json:"web,omitempty"`
}

// Asset is one guest architecture's release asset: a gzip of the scion
// binary. SHA256 and Size are the asset's (the entry checksums.txt lists);
// BinarySHA256 and BinarySize are the binary's once decompressed.
type Asset struct {
	Name         string `json:"name"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	BinarySHA256 string `json:"binary_sha256"`
	BinarySize   int64  `json:"binary_size"`
}

// maxReleaseBytes bounds every size the manifest may name, so a bad manifest
// cannot ask for an unbounded download.
const maxReleaseBytes = 1 << 30

var (
	releaseVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
	assetNameRe      = regexp.MustCompile(`^scion_[0-9A-Za-z.-]+_linux_(amd64|arm64)\.gz$`)
	webAssetNameRe   = regexp.MustCompile(`^scion-web_[0-9A-Za-z.-]+\.tar\.gz$`)
	sha256Re         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexRe            = regexp.MustCompile(`^[0-9a-f]+$`)
)

// validate refuses a manifest that is incomplete or names anything outside
// the shapes lever downloads: it is generated, but it decides a URL and what
// runs in the jail.
func (r *Release) validate() error {
	if !releaseVersionRe.MatchString(r.LeverVersion) {
		return fmt.Errorf("lever version %q", r.LeverVersion)
	}
	if r.ScionVersion == "" || len(r.Commit) != 40 || !hexRe.MatchString(r.Commit) {
		return fmt.Errorf("scion version %q, commit %q", r.ScionVersion, r.Commit)
	}
	if len(r.Assets) == 0 {
		return errors.New("no assets")
	}
	for arch, a := range r.Assets {
		m := assetNameRe.FindStringSubmatch(a.Name)
		switch {
		case m == nil || m[1] != arch:
			return fmt.Errorf("asset %q for %s", a.Name, arch)
		case !sha256Re.MatchString(a.SHA256) || !sha256Re.MatchString(a.BinarySHA256):
			return fmt.Errorf("asset %s: bad sha256", a.Name)
		case a.Size <= 0 || a.Size > maxReleaseBytes || a.BinarySize <= 0 || a.BinarySize > maxReleaseBytes:
			return fmt.Errorf("asset %s: bad size", a.Name)
		}
	}
	if w := r.Web; w != nil {
		switch {
		case !webAssetNameRe.MatchString(w.Name) || w.Name != WebAssetName(r.ScionVersion):
			return fmt.Errorf("web asset %q", w.Name)
		case !sha256Re.MatchString(w.SHA256):
			return fmt.Errorf("web asset %s: bad sha256", w.Name)
		case w.Size <= 0 || w.Size > MaxWebArchiveBytes:
			return fmt.Errorf("web asset %s: bad size", w.Name)
		}
	}
	return nil
}

// Matches reports whether scion.version names the scion this release was
// built from: the pin as written, its module version, or a prefix of at
// least 7 hex digits of its commit.
func (r *Release) Matches(version string) bool {
	if r == nil || version == "" {
		return false
	}
	if version == r.ScionVersion || version == r.ModuleVersion {
		return true
	}
	return len(version) >= 7 && hexRe.MatchString(version) && strings.HasPrefix(r.Commit, version)
}

// releaseFrom reads and validates the manifest in fsys. A missing manifest is
// (nil, nil): this lever carries no release.
func releaseFrom(fsys fs.FS) (*Release, error) {
	b, err := fs.ReadFile(fsys, path.Join(ReleaseDir, ReleaseManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Release
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("embedded scion release manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("embedded scion release manifest: data after the manifest")
	}
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("embedded scion release manifest: %w", err)
	}
	return &r, nil
}

var embeddedRelease = sync.OnceValues(func() (*Release, error) { return releaseFrom(releaseFS) })

// currentRelease is the manifest Resolve uses. A variable so tests can
// substitute one.
var currentRelease = embeddedRelease

func downloadsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DownloadEnv))) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// DownloadsRelease reports whether Resolve downloads this release's prebuilt
// scion for version (on goarch) instead of building it. `lever doctor` asks
// it to know whether a Go toolchain is needed.
func DownloadsRelease(version, goarch string) bool {
	rel, err := currentRelease()
	if err != nil || downloadsDisabled() {
		return false
	}
	_, ok := rel.assetFor(version, goarch)
	return ok
}

// releaseSite is where release assets are downloaded from. Both the first
// URL and every redirect must be https to one of hosts; GitHub answers a
// release download with a redirect to its asset storage.
type releaseSite struct {
	base  string
	hosts map[string]bool
	// transport is nil in production (http.DefaultTransport's settings, with
	// the timeouts below).
	transport http.RoundTripper
}

var githubReleases = releaseSite{
	base: "https://github.com/stevegeek/lever/releases/download/",
	hosts: map[string]bool{
		"github.com":                           true,
		"objects.githubusercontent.com":        true,
		"release-assets.githubusercontent.com": true,
	},
}

// site is the releaseSite Resolve downloads from. A variable so tests can
// point it at a local server.
var site = githubReleases

func (s releaseSite) allowed(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || !s.hosts[u.Host] {
		return fmt.Errorf("refusing to download scion from %s://%s: only https from the lever release hosts", u.Scheme, u.Host)
	}
	return nil
}

func (s releaseSite) client() *http.Client {
	rt := s.transport
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		t.TLSHandshakeTimeout = 20 * time.Second
		t.ResponseHeaderTimeout = 60 * time.Second
		rt = t
	}
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return s.allowed(req.URL)
		},
	}
}

// errReleaseUnavailable marks a download that did not happen (no network,
// the release or asset missing, a dropped connection): Resolve then builds
// from source. Any other download error is a refusal and stops the apply.
var errReleaseUnavailable = errors.New("the release's prebuilt scion is unavailable")

// ErrReleaseTampered is a download whose bytes are not the ones this lever
// pinned. lever refuses it and does not fall back: a release host serving
// other bytes for a pinned asset needs a human to look.
var ErrReleaseTampered = errors.New("the downloaded scion does not match the digest pinned in this lever")

// errNoRelease is resolveRelease's answer when there is nothing to
// download: no manifest in this lever, another scion version, no asset for
// the arch, or downloads turned off.
var errNoRelease = fmt.Errorf("%w: no prebuilt scion for this version", errReleaseUnavailable)

// resolveRelease puts this release's prebuilt scion for version and goarch
// at out. An error wrapping errReleaseUnavailable means: build it instead.
func resolveRelease(ctx context.Context, version, goarch, out string) error {
	rel, err := currentRelease()
	if err != nil {
		return err
	}
	a, ok := rel.assetFor(version, goarch)
	if !ok || downloadsDisabled() {
		return errNoRelease
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fmt.Errorf("scion build dir: %w", err)
	}
	err = fetchRelease(ctx, rel, a, goarch, out)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case errors.Is(err, errReleaseUnavailable):
		progress.From(ctx).Note("prebuilt scion not downloaded; using a source build")
	case errors.Is(err, ErrReleaseTampered):
		return fmt.Errorf("%w (set %s=off to build scion from source instead)", err, DownloadEnv)
	}
	return err
}

func (r *Release) assetFor(version, goarch string) (Asset, bool) {
	if !r.Matches(version) {
		return Asset{}, false
	}
	a, ok := r.Assets[goarch]
	return a, ok
}

// fetchRelease puts the release's verified scion for goarch at out, reusing
// out when it already holds exactly that binary.
func fetchRelease(ctx context.Context, rel *Release, a Asset, goarch, out string) error {
	if fileHasDigest(out, a.BinarySize, a.BinarySHA256) {
		progress.From(ctx).Note("prebuilt scion " + rel.ScionVersion + ", cached")
		return nil
	}
	progress.From(ctx).Detail("downloading the prebuilt scion of lever " + rel.LeverVersion)
	dir := filepath.Dir(out)
	gz, err := os.CreateTemp(dir, ".scion-download-*")
	if err != nil {
		return fmt.Errorf("scion download: %w", err)
	}
	defer func() { _ = os.Remove(gz.Name()) }()
	defer func() { _ = gz.Close() }()
	if err := download(ctx, site, rel, a, gz); err != nil {
		return err
	}
	if _, err := gz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("scion download: %w", err)
	}

	bin, err := os.CreateTemp(dir, ".scion-unpack-*")
	if err != nil {
		return fmt.Errorf("scion download: %w", err)
	}
	binPath := bin.Name()
	defer func() { _ = os.Remove(binPath) }()
	err = unpack(gz, bin, a)
	if cerr := bin.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("scion download: %w", cerr)
	}
	if err != nil {
		return err
	}
	if err := VerifyELFArch(binPath, goarch); err != nil {
		return fmt.Errorf("%w: %v", ErrReleaseTampered, err)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return fmt.Errorf("scion download: %w", err)
	}
	if err := os.Remove(stampPath(out)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scion build stamp: %w", err)
	}
	if err := os.Rename(binPath, out); err != nil {
		return fmt.Errorf("scion download: %w", err)
	}
	progress.From(ctx).Note("prebuilt scion " + rel.ScionVersion + ", downloaded")
	return nil
}

// download writes asset a of rel into w and checks its size and sha256
// against the manifest. It reads at most one byte more than the pinned size.
func download(ctx context.Context, s releaseSite, rel *Release, a Asset, w io.Writer) error {
	u, err := url.Parse(s.base + "v" + rel.LeverVersion + "/" + a.Name)
	if err != nil {
		return fmt.Errorf("scion download URL: %w", err)
	}
	if err := s.allowed(u); err != nil {
		return fmt.Errorf("%w: %v", errReleaseUnavailable, err)
	}
	// A body that stops arriving ends the download (and the apply falls
	// back to the build) instead of holding the apply forever.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("scion download: %w", err)
	}
	// The asset is a .gz file, not a gzip transfer: identity keeps the
	// transport from decoding it before the digest check.
	req.Header.Set("Accept-Encoding", "identity")
	// A refused redirect is unavailable too, not tampering: the bytes are
	// pinned, so a host lever does not know yet costs a source build, not a
	// stopped apply.
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errReleaseUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: GET %s: %s", errReleaseUnavailable, u, resp.Status)
	}
	if resp.ContentLength > a.Size {
		return fmt.Errorf("%w: %s is %d bytes, pinned %d", ErrReleaseTampered, a.Name, resp.ContentLength, a.Size)
	}
	h := sha256.New()
	body := progressReader{r: resp.Body, onRead: func() { stall.Reset(stallTimeout) }}
	fw := &fileWriter{w: w}
	n, err := io.Copy(io.MultiWriter(fw, h), io.LimitReader(body, a.Size+1))
	if fw.err != nil {
		return fmt.Errorf("scion download: write: %w", fw.err)
	}
	if err != nil {
		return fmt.Errorf("%w: GET %s: %v", errReleaseUnavailable, u, err)
	}
	// A body that ends early is a broken transfer, like one that errors;
	// only more bytes than pinned, or other bytes, are tampering.
	if n < a.Size {
		return fmt.Errorf("%w: GET %s: %d of %d bytes", errReleaseUnavailable, u, n, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != a.Size || got != a.SHA256 {
		return fmt.Errorf("%w: %s is %d bytes with sha256 %s, pinned %d bytes with sha256 %s",
			ErrReleaseTampered, a.Name, n, got, a.Size, a.SHA256)
	}
	return nil
}

// stallTimeout is how long a download may go without receiving a byte.
var stallTimeout = 60 * time.Second

type progressReader struct {
	r      io.Reader
	onRead func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead()
	}
	return n, err
}

// fileWriter keeps a local write error (a full disk) apart from the
// network's and the archive's, which decide between fallback and refusal.
type fileWriter struct {
	w   io.Writer
	err error
}

func (f *fileWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

// unpack decompresses a verified asset into w and checks the binary against
// its pinned size and sha256.
func unpack(r io.Reader, w io.Writer, a Asset) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrReleaseTampered, a.Name, err)
	}
	h := sha256.New()
	fw := &fileWriter{w: w}
	n, err := io.Copy(io.MultiWriter(fw, h), io.LimitReader(zr, a.BinarySize+1))
	if fw.err != nil {
		return fmt.Errorf("scion download: write: %w", fw.err)
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrReleaseTampered, a.Name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != a.BinarySize || got != a.BinarySHA256 {
		return fmt.Errorf("%w: %s unpacks to %d bytes with sha256 %s, pinned %d bytes with sha256 %s",
			ErrReleaseTampered, a.Name, n, got, a.BinarySize, a.BinarySHA256)
	}
	return nil
}

// fileHasDigest reports whether p is a regular file of size bytes whose
// sha256 is sum. It hashes the whole file every time: the file is what
// lever installs as root in the guest, and a size or mtime check would take
// any file of the right length.
func fileHasDigest(p string, size int64, sum string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != size {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sum
}
