// Package loginfwd builds lever-login-forward, the guest-side TCP forwarder of
// lever's remote-access login path (main/main.go holds the program and the
// reasoning behind it). The build runs on the OPERATOR HOST, at apply time,
// from a copy of the program's own source.
//
// # How this differs from the other in-jail binaries
//
// lever-agent and lever-manager are cross-compiled ONCE, at image build time
// (`make lever-image`), and baked into the agent image. The forwarder is not:
// it is compiled on every apply of every remote-enabled instance, which means
// every host that runs `lever apply` with remote access on needs a Go
// toolchain — including a host that installs scion from a prebuilt binary
// precisely to avoid one (issue #27).
//
// Why it is shaped this way today:
//
//   - The forwarder does not run in an agent container; it runs in the JAIL
//     VM itself, beside the scion binary, so the agent image is not a vehicle
//     that reaches it.
//   - lever is normally an installed BINARY with no source tree beside it,
//     while the guest needs a linux executable for its own architecture. The
//     source is therefore embedded and built where lever runs.
//   - The program is stdlib-only, so the build needs a Go toolchain and
//     nothing else: no module download, no network.
//
// The alternative is to cross-compile it at image build time like lever-agent
// and install it into the jail from the image build context (or from a release
// asset), which would remove the apply-time Go requirement. That is a larger
// change to the image and release pipeline and is NOT made here.
//
// Build is idempotent and cheap to repeat: -trimpath keeps the output
// independent of the build directory, so the guest-side hash-skip
// (internal/backend/guest) compares like with like across hosts and runs.
package loginfwd

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision"
)

//go:embed main/main.go
var source string

// goMod is the module file the embedded source is built with. The Go directive
// is deliberately conservative: the program uses nothing newer, and a floor
// above the host's toolchain would fail the build for no reason.
const goMod = "module lever-login-forward\n\ngo 1.22\n"

// Build cross-compiles the forwarder for goarch ("arm64"/"amd64") and returns
// the host-local path of the result. machine names the build directory under
// os.TempDir so two jails on one host never share one.
func Build(ctx context.Context, r proc.Runner, goarch, machine string) (string, error) {
	dir := filepath.Join(os.TempDir(), "lever-loginfwd-"+machine)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("login forwarder build dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		return "", fmt.Errorf("stage login forwarder source: %w", err)
	}
	// A module of its own: the source is compiled outside lever's module, and
	// go build refuses to work without one. Nothing is ever downloaded for it.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		return "", fmt.Errorf("stage login forwarder go.mod: %w", err)
	}
	goBin, err := provision.GoBinary(ctx, r)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "lever-login-forward")
	if _, err := r.RunIn(ctx, dir, map[string]string{"GOOS": "linux", "GOARCH": goarch, "CGO_ENABLED": "0", "GOFLAGS": "-mod=mod", "GOPROXY": "off"},
		goBin, "build", "-trimpath", "-o", out, "."); err != nil {
		return "", fmt.Errorf("cross-compile the login forwarder (remote access needs a Go toolchain on this host): %w", err)
	}
	return out, nil
}
