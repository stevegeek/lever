// Package scionbin produces a host-local scion binary for a guest's
// architecture: an operator-supplied prebuilt binary verified as-is, a host
// source checkout cross-compiled, or a pinned module version fetched through
// the Go module system and cross-compiled. It is the ONLY place in lever that
// knows how scion is built.
//
// The build runs on the HOST: the guest carries no toolchain, and Go's build
// cache makes re-runs incremental. Installing the result into the guest is
// internal/backend/guest's job.
package scionbin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision"
)

// ModulePath is the upstream scion Go module.
const ModulePath = "github.com/GoogleCloudPlatform/scion"

// Spec names the one place lever should get scion from. At most one of
// Binary/Source/Version is set; config validation enforces that
// (internal/config). A struct rather than positional strings because three
// same-typed parameters across two call sites is a mis-ordering waiting to
// happen.
type Spec struct {
	// Binary is a host-local, already-built linux binary, installed as-is. No
	// Go toolchain, module cache or egress is needed on this host.
	Binary string
	// Source is a host checkout to cross-compile.
	Source string
	// Version pins a scion module version/commit to fetch and cross-compile.
	Version string
	// WebUI additionally builds scion's SPA on the host and stages it into the
	// guest, so the hub can serve the web UI (internal/provision/webassets).
	// A field on the spec rather than a parameter on EnsureScion so the two
	// backends' copies of the provisioning block stay one struct literal: that
	// block has drifted before — Binary was added to both literals while the
	// guard around them was updated in neither (see backend.Config.HasScion).
	WebUI bool
	// Prebuilt, when set, is this spec's build already started on the host
	// while the guest came up (StartPrebuild). It is not configuration: the
	// guest's scion step takes its result (Prebuild.Result) instead of
	// building again, when the guest's architecture matches.
	Prebuilt *Prebuild
}

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
func (s Spec) BuildsWebAssets() bool {
	return s.WebUI && s.Binary == "" && (s.Source != "" || s.Version != "")
}

// Validate fails on a plainly wrong spec without touching a toolchain or a
// guest, so a bad config fails without a round-trip and without depending on
// the guest being up.
func (s Spec) Validate() error {
	if s.Binary == "" && s.Source == "" && s.Version == "" {
		return fmt.Errorf("no scion configured: set one of scion.binary, scion.source or scion.version")
	}
	if s.Source != "" {
		fi, err := os.Stat(s.Source)
		if err != nil {
			return &SourceError{Path: s.Source, Err: err}
		}
		if !fi.IsDir() {
			return &SourceError{Path: s.Source}
		}
	}
	return nil
}

// SourceError reports that Spec.Source cannot be built from: Path is the
// configured scion.source; Err is the stat failure, or nil when the path
// exists but is not a directory. Callers match it with errors.As.
type SourceError struct {
	Path string
	Err  error
}

func (e *SourceError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("scion source %q is not a directory", e.Path)
	}
	return fmt.Sprintf("scion source %q: %v", e.Path, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }

// buildFlags strip the symbol table and DWARF (-s -w) and the host's file
// paths (-trimpath) from the cross-compiled scion: about 173 MB to 121 MB
// (lever#191), less to stream into the guest and to page in at each hub
// start. Go's panic stack traces keep their function names and lines
// (pclntab stays); only debugger and symbolizer data goes.
var buildFlags = []string{"-trimpath", "-ldflags=-s -w"}

// Resolve produces a host-local scion binary for goarch ("arm64"/"amd64").
// machine names the output file under OutputDir so two jails on one host never
// share a build output.
//
// The Binary branch returns before any toolchain is touched, which is what
// lets the machine hosting the jail carry no Go, no module cache and no
// egress at all (issue #27). Callers run Validate first; Resolve does it
// again so it is safe on its own.
func Resolve(ctx context.Context, r proc.Runner, spec Spec, goarch, machine string) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	if spec.Binary != "" {
		if err := VerifyELFArch(spec.Binary, goarch); err != nil {
			return "", err
		}
		return spec.Binary, nil
	}

	out, err := OutputPath(machine)
	if err != nil {
		return "", err
	}
	goBin := "go"
	buildDir := spec.Source
	stampKey := ""
	if spec.Version != "" {
		gb, err := provision.GoBinary(ctx, r)
		if err != nil {
			return "", err
		}
		// A pinned module's source never changes, so the last output is
		// this build's when the stamp beside it says so (lever#187). Only
		// for a version that names one source for good: a branch name or
		// "latest" moves, and must be fetched every time.
		if immutableVersion(spec.Version) {
			stampKey = buildKey(spec.Version, goarch, gb)
		}
		if stampKey != "" && stampMatches(out, stampKey) {
			return out, nil
		}
		dir, err := fetchModule(ctx, r, gb, spec.Version)
		if err != nil {
			return "", err
		}
		goBin, buildDir = gb, dir
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", fmt.Errorf("scion build dir: %w", err)
	}
	// The stamp goes before the build: a build that fails, or that is
	// interrupted, must not leave an old stamp naming a new output.
	if err := os.Remove(stampPath(out)); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("scion build stamp: %w", err)
	}
	if _, err := r.RunIn(ctx, buildDir, map[string]string{"GOOS": "linux", "GOARCH": goarch},
		goBin, buildArgs(out)...); err != nil {
		return "", fmt.Errorf("cross-compile scion: %w", err)
	}
	if stampKey != "" {
		writeStamp(out, stampKey) // best-effort: a missing stamp costs one build
	}
	return out, nil
}

// immutableRe matches the versions that name one module source for good: a
// commit hash (or a prefix of at least 7 hex digits) and a semantic or
// pseudo-version (v1.2.3, v0.0.0-20260101000000-abcdef123456, v1.2.3-rc.1).
var immutableRe = regexp.MustCompile(`^(?:[0-9a-f]{7,40}|v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$`)

// immutableVersion reports whether a scion.version may be stamped. A
// short hex prefix resolves to the same commit for as long as it is
// unambiguous; the module proxy and go.sum keep a version's content fixed.
func immutableVersion(v string) bool { return immutableRe.MatchString(v) }

// buildArgs is the go command line that builds scion into out.
func buildArgs(out string) []string {
	return append(append([]string{"build"}, buildFlags...), "-o", out, "./cmd/scion")
}

// buildEnvKeys are the environment variables that change what `go build`
// produces from the same module and toolchain. They are part of the stamp's
// key, so a change to any of them rebuilds.
var buildEnvKeys = []string{"GOFLAGS", "GOTOOLCHAIN", "GOEXPERIMENT", "GOAMD64", "GOARM64", "CGO_ENABLED", "GODEBUG"}

// buildKey names a Version-mode build: the pin, the arch, the toolchain
// (its GOROOT and the version in GOROOT/VERSION), the build command line and
// buildEnvKeys. "" when the toolchain version cannot be read: the build then
// runs every time, as it did before the stamp.
func buildKey(version, goarch, goBin string) string {
	root := filepath.Dir(filepath.Dir(goBin))
	v, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return ""
	}
	goVersion, _, _ := strings.Cut(string(v), "\n")
	if goVersion = strings.TrimSpace(goVersion); goVersion == "" {
		return ""
	}
	h := sha256.New()
	fields := []string{"lever scion build v1", version, "linux", goarch, root, goVersion, strings.Join(buildArgs("OUT"), " ")}
	for _, k := range buildEnvKeys {
		fields = append(fields, k+"="+os.Getenv(k))
	}
	for _, f := range fields {
		fmt.Fprintf(h, "%d:%s\n", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildStamp records the build that produced an output: its key, and the
// size and modification time the output had when the build finished, so an
// output changed since (rebuilt by hand, truncated, replaced) is rebuilt.
type buildStamp struct {
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime_ns"`
}

func stampPath(out string) string { return out + ".stamp" }

// stampMatches reports whether out exists as the build named key left it.
func stampMatches(out, key string) bool {
	b, err := os.ReadFile(stampPath(out))
	if err != nil {
		return false
	}
	var st buildStamp
	if json.Unmarshal(b, &st) != nil || st.Key != key {
		return false
	}
	fi, err := os.Stat(out)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == st.Size && fi.ModTime().UnixNano() == st.ModTime
}

func writeStamp(out, key string) {
	fi, err := os.Stat(out)
	if err != nil {
		return
	}
	b, err := json.Marshal(buildStamp{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UnixNano()})
	if err != nil {
		return
	}
	tmp := stampPath(out) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, stampPath(out))
	}
}

// OutputDir is the host directory Resolve writes cross-compiled scion
// binaries into: the operator's own cache directory, beside the web-asset
// builds (internal/provision/webassets.CacheRoot), created 0700 by Resolve.
//
// Not os.TempDir. On a Linux host /tmp is shared by every local user, and a
// fixed name there is a path anyone can pre-create — and then swap out
// between the host-side hash and the root install into the jail
// (internal/backend/guest). Under the user's cache dir nobody else can write
// the path. There is no build cache to protect: Go's own build cache makes
// the cross-compile incremental whatever the output path, so the only thing
// that lives here is the last output for each machine.
func OutputDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir for the scion build: %w", err)
	}
	return filepath.Join(cache, "lever", "scion-bin"), nil
}

// OutputPath is where Resolve writes a cross-compiled scion for machine.
func OutputPath(machine string) (string, error) {
	dir, err := OutputDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lever-scion-"+machine), nil
}

// FetchModule downloads the pinned scion module via the Go module system and
// returns (goBinary, moduleSourceDir) for the cross-compile. It uses the REAL
// go binary (provision.GoBinary) for the build because the module cache lives
// outside any toolchain-manager project dir — a version manager that resolves
// `go` by walking up for a project file (asdf) cannot resolve it from the
// read-only module cache, whereas the absolute binary always works.
func FetchModule(ctx context.Context, r proc.Runner, version string) (goBin, dir string, err error) {
	goBin, err = provision.GoBinary(ctx, r)
	if err != nil {
		return "", "", err
	}
	dir, err = fetchModule(ctx, r, goBin, version)
	if err != nil {
		return "", "", err
	}
	return goBin, dir, nil
}

// fetchModule is FetchModule with the go binary already resolved.
func fetchModule(ctx context.Context, r proc.Runner, goBin, version string) (string, error) {
	out, err := r.Run(ctx, nil, goBin, "mod", "download", "-json", ModulePath+"@"+version)
	if err != nil {
		return "", fmt.Errorf("download scion %s: %w", version, err)
	}
	var dl struct{ Dir, Error string }
	if jerr := json.Unmarshal([]byte(out.Stdout), &dl); jerr != nil {
		return "", fmt.Errorf("parse `go mod download` output for scion %s: %w", version, jerr)
	}
	if dl.Error != "" {
		return "", fmt.Errorf("download scion %s: %s", version, dl.Error)
	}
	if dl.Dir == "" {
		return "", fmt.Errorf("`go mod download` returned no source dir for scion %s", version)
	}
	return dl.Dir, nil
}

// ErrShallowSource is SourceHasCommit's error for a shallow clone that does
// not show the commit: its history is cut off, so a missing commit says
// nothing either way. Unlike SourceHasCommit's other errors it is not "cannot
// tell, carry on": a caller gating on the commit refuses it, since the commit
// cannot be shown and a clone can be deepened (git fetch --unshallow).
var ErrShallowSource = errors.New("the checkout is a shallow clone that does not show the commit (run git fetch --unshallow)")

// SourceHasCommit reports whether the source checkout's HEAD holds commit
// (git merge-base --is-ancestor).
//
//   - true: commit is an ancestor of HEAD (also trustworthy in a shallow
//     clone: git found the path).
//   - false, nil: the checkout is a full repository and either commit is not
//     an ancestor of HEAD or git has no such object at all (a checkout that
//     predates it and was never fetched, or a fork that never had it).
//   - false, ErrShallowSource: a shallow clone that does not show it.
//   - any other error: git cannot tell (not a repository, git not runnable,
//     the deadline). Callers treat that as a warning, not a refusal.
//
// source is passed as git's -C value and commit as a revision argument;
// nothing goes through a shell.
func SourceHasCommit(ctx context.Context, r proc.Runner, source, commit string) (bool, error) {
	res, err := r.Run(ctx, nil, "git", "-C", source, "merge-base", "--is-ancestor", commit, "HEAD")
	if err == nil {
		return true, nil
	}
	cannotTell := fmt.Errorf("git merge-base in %s: %w", source, err)
	if ctx.Err() != nil {
		return false, cannotTell
	}
	// Exit 1 is "not an ancestor"; anything else (128: an object git does
	// not have, or not a repository) needs a closer look before it counts.
	notAncestor := res.Code == 1
	if _, gerr := r.Run(ctx, nil, "git", "-C", source, "rev-parse", "--git-dir"); gerr != nil {
		return false, cannotTell
	}
	if !notAncestor {
		if _, cerr := r.Run(ctx, nil, "git", "-C", source, "cat-file", "-e", commit+"^{commit}"); cerr == nil || ctx.Err() != nil {
			// git has the commit yet merge-base failed, or the deadline hit.
			return false, cannotTell
		}
	}
	sh, serr := r.Run(ctx, nil, "git", "-C", source, "rev-parse", "--is-shallow-repository")
	if serr != nil {
		return false, cannotTell
	}
	switch strings.TrimSpace(sh.Stdout) {
	case "false":
		return false, nil
	case "true":
		return false, fmt.Errorf("%s: %w", source, ErrShallowSource)
	}
	return false, cannotTell
}
