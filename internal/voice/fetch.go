package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrUnpinned: the model's table entry is not verified (Model.Pinned).
var ErrUnpinned = errors.New("model not pinned")

// ErrModel: the model file on disk is missing, unsafe, or not the pinned
// file.
var ErrModel = errors.New("model file")

// HuggingFaceHost reports whether u is one of Hugging Face's own hosts, over
// https on the default port: the site itself and its content hosts (a
// download is redirected from huggingface.co to a CDN host under
// huggingface.co or hf.co). Every other host is refused, the first request
// and every redirect alike.
func HuggingFaceHost(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "huggingface.co" || strings.HasSuffix(h, ".huggingface.co") || strings.HasSuffix(h, ".hf.co")
}

// GitHubReleaseHost reports whether u is a host GitHub serves release
// assets from, over https on the default port: github.com, which answers a
// download with a redirect, and the content hosts it redirects to
// (release-assets.githubusercontent.com, and objects.githubusercontent.com
// before it). Not any other *.githubusercontent.com host: those serve
// repository content. Every other host is refused, the first request and
// every redirect alike. Speech model archives come from there.
func GitHubReleaseHost(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

// Fetcher downloads a pinned model. The zero value is the real one.
type Fetcher struct {
	// Client is the HTTP client; nil = one with connection timeouts and no
	// overall limit (a large model takes a while). Its CheckRedirect is
	// replaced.
	Client *http.Client
	// AllowHost decides every host the download may reach; nil =
	// HuggingFaceHost for a Whisper model, GitHubReleaseHost for a speech
	// model's archive. Tests allow their own loopback server.
	AllowHost func(*url.URL) bool
	// Progress, when set, gets a line every 10 per cent.
	Progress io.Writer
}

const maxRedirects = 10

// Fetch downloads m into dir unless a verified copy is there already, and
// returns its path. The body goes to a private temp file in dir; only a
// file of the pinned size and sha256 is renamed into place, mode 0600.
func (f Fetcher) Fetch(ctx context.Context, m Model, dir string) (string, error) {
	return f.FetchFrom(ctx, m, m.URL(), dir)
}

// FetchFrom is Fetch from src instead of the table's URL (tests).
func (f Fetcher) FetchFrom(ctx context.Context, m Model, src, dir string) (string, error) {
	if why := m.Pinned(); why != "" {
		return "", fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	if err := PrivateDir(dir); err != nil {
		return "", err
	}
	final := filepath.Join(dir, m.File)
	if err := Verify(final, m); err == nil {
		return final, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A file that is there but wrong is replaced below, never used.
		f.say("%s: the file there does not verify (%v); downloading again\n", m.Name, err)
	}
	tmp, err := f.download(ctx, download{name: m.Name, src: src, size: m.Size, sha256: m.SHA256,
		allow: HuggingFaceHost, hosts: "a Hugging Face host"}, dir)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return final, nil
}

// download is one pinned file to download: its name for messages, its
// URL, size and sha256, and the hosts it may come from (allow, unless the
// Fetcher's AllowHost is set; hosts names them in a refusal).
type download struct {
	name, src string
	size      int64
	sha256    string
	allow     func(*url.URL) bool
	hosts     string
}

// download fetches d into a private temp file (.fetch-*, mode 0600) in
// dir and returns its path once its size and sha256 match the pins; the
// caller renames or removes it. Nothing is left behind on an error.
func (f Fetcher) download(ctx context.Context, d download, dir string) (string, error) {
	allow := d.allow
	if f.AllowHost != nil {
		allow = f.AllowHost
	}
	u, err := url.Parse(d.src)
	if err != nil || !allow(u) {
		return "", fmt.Errorf("voice: refusing to download %s from %s: not %s", d.name, d.src, d.hosts)
	}
	client := http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
	}}
	if f.Client != nil {
		client = *f.Client
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if !allow(req.URL) {
			return fmt.Errorf("refusing a redirect to %s: not %s", req.URL.Host, d.hosts)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.src, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice: download %s: %w", d.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("voice: download %s: HTTP %d", d.name, resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != d.size {
		return "", fmt.Errorf("voice: download %s: the server announces %d bytes, the pinned size is %d", d.name, resp.ContentLength, d.size)
	}
	removeStaleFetches(dir, time.Now())
	tmp, err := os.CreateTemp(dir, ".fetch-*")
	if err != nil {
		return "", err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), &progressReader{r: io.LimitReader(resp.Body, d.size+1), total: d.size, f: f, name: d.name})
	if err != nil {
		return "", fmt.Errorf("voice: download %s: %w", d.name, err)
	}
	if n != d.size {
		return "", fmt.Errorf("voice: download %s: got %d bytes, the pinned size is %d", d.name, n, d.size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != d.sha256 {
		return "", fmt.Errorf("voice: download %s: sha256 %s, the pinned sha256 is %s", d.name, got, d.sha256)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	done = true
	return tmp.Name(), nil
}

func (f Fetcher) say(format string, a ...any) {
	if f.Progress != nil {
		fmt.Fprintf(f.Progress, format, a...)
	}
}

// progressReader reports every 10 per cent read.
type progressReader struct {
	r           io.Reader
	total, read int64
	last        int64
	f           Fetcher
	name        string
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.total > 0 {
		if pct := p.read * 100 / p.total; pct/10 > p.last/10 {
			p.last = pct
			p.f.say("%s: %d%%\n", p.name, pct)
		}
	}
	return n, err
}

// PrivateDir makes dir (0700) if it is missing, and refuses one that is a
// symbolic link, not a directory, open to other users, or not the caller's.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("voice: %s is not a directory (a symbolic link?)", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("voice: %s is %v: other users can reach it; chmod 700 it", dir, fi.Mode().Perm())
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return fmt.Errorf("voice: %s belongs to uid %d, not to you", dir, owner)
	}
	return nil
}

// Verify checks the model file at p: a regular file of the caller's (no
// symbolic link), not writable by others, of the pinned size and sha256.
// A missing file is fs.ErrNotExist.
func Verify(p string, m Model) error {
	if why := m.Pinned(); why != "" {
		return fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	return verifyFile(p, m.Size, m.SHA256)
}

// verifyFile checks the file at p: a regular file of the caller's (no
// symbolic link), not writable by others, of size bytes and sha256 sum.
// A missing file is fs.ErrNotExist.
func verifyFile(p string, size int64, sum string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	switch {
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file (a symbolic link?)", ErrModel, p)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%w: %s is %v: another user can change it", ErrModel, p, fi.Mode().Perm())
	case fi.Size() != size:
		return fmt.Errorf("%w: %s has %d bytes, the pinned size is %d", ErrModel, p, fi.Size(), size)
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d, not to you", ErrModel, p, owner)
	}
	file, err := os.Open(p)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%w: %s has sha256 %s, the pinned sha256 is %s", ErrModel, p, got, sum)
	}
	return nil
}

// ModelPath is where m lives in dir.
func ModelPath(dir string, m Model) string { return filepath.Join(dir, m.File) }

// CheckServer checks the whisper-server program at p before lever runs it:
// an absolute path whose real file is a regular, executable file owned by
// the caller or root and writable by nobody else, in a directory nobody
// else may write to. It returns the real path.
func CheckServer(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("voice: -server %q is not an absolute path", p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("voice: -server: %w", err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("voice: -server: %w", err)
	}
	if err := safeOwned(real, fi); err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("voice: -server %s is not an executable file", real)
	}
	dir := filepath.Dir(real)
	di, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("voice: -server: %w", err)
	}
	if err := safeOwned(dir, di); err != nil {
		return "", err
	}
	return real, nil
}

// safeOwned refuses a path writable by group or others, or owned by
// someone other than the caller or root.
func safeOwned(p string, fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("voice: -server: %s is %v: another user can replace what lever runs", p, fi.Mode().Perm())
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() && owner != 0 {
		return fmt.Errorf("voice: -server: %s belongs to uid %d, neither you nor root", p, owner)
	}
	return nil
}

// removeStaleFetches deletes what a killed fetch left in dir: partial
// downloads (regular files named .fetch-*) and partly unpacked speech
// models (private directories named .fetch-*), untouched for a day (a
// fetch running now writes continuously). Best effort.
func removeStaleFetches(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".fetch-") || !e.Type().IsRegular() && !e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
