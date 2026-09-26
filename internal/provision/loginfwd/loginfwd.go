// Package loginfwd provides lever-login-forward, the guest-side TCP forwarder
// of lever's remote-access login path (main/main.go holds the program and the
// reasoning behind it), for the guest's architecture.
//
// # Where the binary comes from
//
// Two sources, tried in this order (Forwarder):
//
//  1. PREBUILT, embedded in this lever binary. `make install` and the release
//     workflow run genprebuilt before building lever, which cross-compiles the
//     forwarder for every guest architecture lever supports (linux/amd64 and
//     linux/arm64) into prebuilt/ and writes a digest of the source it was
//     built from. `//go:embed all:prebuilt` then carries those files in the
//     lever binary. A host that runs such a lever needs no Go toolchain for
//     remote access (issue #38): scion.binary mode, whose whole point is a
//     build-free host, no longer has to install Go for this one program.
//  2. BUILT AT APPLY TIME on the operator host, from the embedded source, as
//     every lever before this one did. This is the fallback for a lever built
//     without the generate step (`go build`, `go install ...@version`, a dev
//     checkout), and it needs a Go toolchain — which anyone who could build
//     lever that way already has.
//
// Why embedding rather than the alternatives:
//
//   - A `remote.forwarder_binary:` path, like scion.binary, would push the
//     cross-compile onto every operator and add a second artefact to keep in
//     step with lever's own version. The forwarder has no configuration and no
//     reason to differ between installs; it belongs to lever, so it ships with
//     lever.
//   - An existing guest tool (socat, python, nc) would remove the compiled
//     program altogether, but none is safe to rely on. The guest carries
//     OpenBSD netcat only (see internal/remoteproxy/jaildial.go), and its
//     listen mode serves ONE connection and has no fork: the hub makes three
//     back-to-back requests per login (discovery, /token, /userinfo), so a
//     restart loop around `nc -l` would race and drop them. socat would work,
//     but it is not installed, and adding a guest package changes the jail's
//     prereqs on every backend for a component lever already has, with a
//     live-validated history, in Go.
//   - A build tag would make the embed opt-in, and a lever built without it
//     would silently need Go again. `all:prebuilt` embeds whatever the
//     directory holds (at least its .gitkeep), so no tag is needed, and the
//     digest check below decides whether the contents are usable.
//
// # Staleness
//
// A prebuilt binary is used only when prebuilt/source.sha256 equals
// SourceDigest(): a digest of the embedded source, the module file and the
// build flags. A dev checkout whose prebuilt/ was generated before main.go
// changed therefore falls back to building from source instead of shipping an
// old forwarder. genprebuilt writes the digest LAST, so an interrupted
// generation leaves no digest and is ignored.
//
// Both paths use one recipe (BuildTo): -trimpath and the same module file keep
// the output independent of the build directory, so the guest-side hash-skip
// (internal/backend/guest) compares like with like.
package loginfwd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision"
)

//go:embed main/main.go
var source string

// prebuiltFS carries prebuilt/ as genprebuilt left it: in a plain checkout only
// its .gitkeep, in a release or `make install` build the gzip-compressed
// forwarders and their digest.
//
//go:embed all:prebuilt
var prebuiltFS embed.FS

// embedded is what Prebuilt reads. A variable so tests can substitute an
// in-memory tree without touching the real directory.
var embedded fs.FS = prebuiltFS

// goMod is the module file the embedded source is built with. The Go directive
// is deliberately conservative: the program uses nothing newer, and a floor
// above the host's toolchain would fail the build for no reason.
const goMod = "module lever-login-forward\n\ngo 1.22\n"

// buildFlags are the `go build` flags both sources use. Part of SourceDigest,
// so a prebuilt binary made with different flags counts as stale. -s -w drop
// the symbol table and DWARF: nobody debugs this program in the guest, and
// they are a third of its size, which matters now that lever embeds two
// copies.
var buildFlags = []string{"-trimpath", "-ldflags=-s -w"}

// GuestArches are the guest architectures lever runs (guest.GOARCH answers one
// of these), and so the set genprebuilt builds and PrebuiltComplete requires.
var GuestArches = []string{"amd64", "arm64"}

const (
	// PrebuiltDir is the directory, relative to this package, that genprebuilt
	// writes into and the embed reads from.
	PrebuiltDir = "prebuilt"
	// DigestFile holds SourceDigest() of the source the prebuilt binaries were
	// built from, as one hex line.
	DigestFile = "source.sha256"
)

// PrebuiltName is the file name, inside PrebuiltDir, of the gzip-compressed
// forwarder for goarch.
func PrebuiltName(goarch string) string { return "lever-login-forward-linux-" + goarch + ".gz" }

// SourceDigest identifies exactly what a prebuilt binary must have been built
// from: the program, its module file, and the build flags.
func SourceDigest() string {
	h := sha256.New()
	for _, part := range []string{source, goMod, strings.Join(buildFlags, " ")} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Prebuilt returns the embedded forwarder for goarch, when this lever carries
// one built from its own source.
func Prebuilt(goarch string) ([]byte, bool) {
	return prebuiltFrom(embedded, goarch)
}

// PrebuiltComplete reports whether this lever carries a current prebuilt
// forwarder for EVERY guest architecture — so that no guest, whichever it
// turns out to be, needs a Go toolchain on the host. Config loading asks this
// before refusing a toolchain-less scion.binary host, and it cannot know the
// guest's architecture yet.
func PrebuiltComplete() bool {
	for _, a := range GuestArches {
		if _, ok := Prebuilt(a); !ok {
			return false
		}
	}
	return true
}

// prebuiltFrom is Prebuilt over any tree shaped like PrebuiltDir's parent.
// Every failure is "not available", never an error: the caller falls back to
// building from source, which is always correct.
func prebuiltFrom(fsys fs.FS, goarch string) ([]byte, bool) {
	digest, err := fs.ReadFile(fsys, path.Join(PrebuiltDir, DigestFile))
	if err != nil || strings.TrimSpace(string(digest)) != SourceDigest() {
		return nil, false
	}
	gz, err := fs.ReadFile(fsys, path.Join(PrebuiltDir, PrebuiltName(goarch)))
	if err != nil {
		return nil, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, false
	}
	bin, err := io.ReadAll(zr)
	if err != nil || len(bin) == 0 {
		return nil, false
	}
	return bin, true
}

// Source says where Forwarder got the binary, for the caller's messages.
type Source string

const (
	SourcePrebuilt Source = "prebuilt"
	SourceBuilt    Source = "built"
)

// Forwarder returns a host-local path to the forwarder for goarch: the
// embedded prebuilt copy when this lever carries a current one, else a fresh
// cross-compile (Build). machine names the host directory, as for Build.
func Forwarder(ctx context.Context, r proc.Runner, goarch, machine string) (string, Source, error) {
	if bin, ok := Prebuilt(goarch); ok {
		out, err := writePrebuilt(machine, bin)
		return out, SourcePrebuilt, err
	}
	out, err := Build(ctx, r, goarch, machine)
	return out, SourceBuilt, err
}

// writePrebuilt stages bin at the same path Build would write, in the same
// 0700 per-user directory (see buildDir for why not os.TempDir).
func writePrebuilt(machine string, bin []byte) (string, error) {
	dir, err := buildDir(machine)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("login forwarder dir: %w", err)
	}
	out := filepath.Join(dir, "lever-login-forward")
	if err := os.WriteFile(out, bin, 0o700); err != nil {
		return "", fmt.Errorf("stage the prebuilt login forwarder: %w", err)
	}
	// WriteFile keeps an existing file's mode; make sure it is executable.
	if err := os.Chmod(out, 0o700); err != nil {
		return "", fmt.Errorf("stage the prebuilt login forwarder: %w", err)
	}
	return out, nil
}

// Build cross-compiles the forwarder for goarch ("arm64"/"amd64") and returns
// the host-local path of the result. machine names the build directory under
// buildDir so two jails on one host never share one.
func Build(ctx context.Context, r proc.Runner, goarch, machine string) (string, error) {
	dir, err := buildDir(machine)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("login forwarder build dir: %w", err)
	}
	out := filepath.Join(dir, "lever-login-forward")
	if err := BuildTo(ctx, r, goarch, dir, out); err != nil {
		return "", err
	}
	return out, nil
}

// BuildTo stages the source in dir (which must exist and be the caller's own)
// and cross-compiles it for linux/goarch to out. Build and genprebuilt share
// it, so the prebuilt and the apply-time binary come from one recipe.
func BuildTo(ctx context.Context, r proc.Runner, goarch, dir, out string) error {
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		return fmt.Errorf("stage login forwarder source: %w", err)
	}
	// A module of its own: the source is compiled outside lever's module, and
	// go build refuses to work without one. Nothing is ever downloaded for it.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		return fmt.Errorf("stage login forwarder go.mod: %w", err)
	}
	goBin, err := provision.GoBinary(ctx, r)
	if err != nil {
		return fmt.Errorf("%w (this lever carries no prebuilt login forwarder, so remote access builds one and needs a Go toolchain; "+
			"a release build or `make install` embeds it)", err)
	}
	args := append(append([]string{"build"}, buildFlags...), "-o", out, ".")
	if _, err := r.RunIn(ctx, dir, map[string]string{"GOOS": "linux", "GOARCH": goarch, "CGO_ENABLED": "0", "GOFLAGS": "-mod=mod", "GOPROXY": "off"},
		goBin, args...); err != nil {
		return fmt.Errorf("cross-compile the login forwarder (this lever carries no prebuilt one, so remote access needs a Go toolchain on this host): %w", err)
	}
	return nil
}

// buildDir is the host directory the forwarder for machine is staged and
// built in: under the operator's own cache directory, beside the scion
// binary and web-asset builds (scionbin.OutputDir, webassets.CacheRoot).
//
// Not os.TempDir. On a Linux host /tmp is shared by every local user, and
// MkdirAll on a fixed name there succeeds on a directory somebody else
// created first — which hands them the source, the module file and the
// output between the host-side hash and the root install into the jail.
// Under the user's cache dir nobody else can write the path.
func buildDir(machine string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir for the login forwarder build: %w", err)
	}
	return filepath.Join(cache, "lever", "loginfwd", machine), nil
}
