// Package buildstamp describes the build of the running binary: its release
// plus the commit it was built from. It imports only the standard library,
// because the in-jail lever-agent links it and must not link internal/cli.
package buildstamp

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
)

// Set with -ldflags -X by the Makefile and the release build. When revision
// is set the pair wins over Go's VCS stamp, which in a git worktree nested in
// another checkout names that checkout's commit.
var (
	revision string // the full commit hash
	modified string // "true" for a build with uncommitted changes
)

// Describe is release plus the build's commit (with "-dirty") or, for a `go
// install module@vX` build, the module version.
//
// A build with no provenance, or a dirty one, also gets the binary's content
// hash: all dirty builds of one commit share a stamp. apply restarts the
// broker and the remote proxy when lever's string changes, so it must differ
// between builds.
func Describe(release string) string {
	p := buildProvenance()
	return format(release, p.rev, p.dirty, p.modVersion, p.bin)
}

type provenance struct {
	rev        string
	dirty      bool
	modVersion string
	bin        string
}

var buildProvenance = sync.OnceValue(func() provenance {
	info, _ := debug.ReadBuildInfo()
	p := fromBuild(info, revision, modified)
	if needsBinaryHash(p.rev, p.dirty, p.modVersion) {
		p.bin = executableHash()
	}
	return p
})

// fromBuild reads the provenance from Go's build info, or from the ldflags
// stamp when stampRev is set. info may be nil.
func fromBuild(info *debug.BuildInfo, stampRev, stampModified string) provenance {
	var p provenance
	if info != nil {
		p.modVersion = info.Main.Version
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				p.rev = s.Value
			case "vcs.modified":
				p.dirty = s.Value == "true"
			}
		}
	}
	if stampRev != "" {
		p.rev, p.dirty = stampRev, stampModified == "true"
	}
	return p
}

// needsBinaryHash reports a build whose stamp does not tell builds apart: no
// commit and no module version, or a commit with uncommitted changes.
func needsBinaryHash(rev string, dirty bool, modVersion string) bool {
	if rev != "" {
		return dirty
	}
	return modVersion == "" || modVersion == "(devel)"
}

// executableHash is the first 12 hex digits of the running binary's SHA-256,
// or "" when the binary cannot be read.
func executableHash() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// A VCS commit takes precedence over a module version; bin is appended when set.
func format(base, rev string, dirty bool, modVersion, bin string) string {
	var parts []string
	switch {
	case rev != "":
		short := rev
		if len(short) > 12 {
			short = short[:12]
		}
		if dirty {
			short += "-dirty"
		}
		parts = append(parts, short)
	case modVersion != "" && modVersion != "(devel)":
		parts = append(parts, modVersion)
	}
	if bin != "" {
		parts = append(parts, "bin "+bin)
	}
	if len(parts) == 0 {
		return base
	}
	return base + " (" + strings.Join(parts, ", ") + ")"
}
