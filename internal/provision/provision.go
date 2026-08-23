// Package provision holds the HOST-side build pipelines that produce the
// artefacts lever puts into a jail: the scion binary (scionbin), scion's web
// UI (webassets) and the remote-access login forwarder (loginfwd). Each
// subpackage takes an exec.Runner and returns a local path; none of them
// knows how to reach a guest. Streaming the artefact in is
// internal/backend/guest's job, through its prefix transport.
//
// The split is the point: a backend's guest helper should own prefixes and
// in-guest scripts, not npm invocations and Go cross-compiles. `lever doctor`
// probes the same toolchains these pipelines use, so the probes live beside
// the pipelines.
//
// This root package carries only what the subpackages share.
package provision

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/exec"
)

// GoBinary resolves the REAL go binary (GOROOT/bin/go) rather than trusting
// `go` on PATH from an arbitrary working directory. A version manager that
// resolves `go` by walking up for a project file (asdf, mise) cannot resolve
// it from a directory outside any project — which is exactly where the module
// cache and the temp build directories these pipelines use live.
func GoBinary(ctx context.Context, r exec.Runner) (string, error) {
	root, err := r.Run(ctx, nil, "go", "env", "GOROOT")
	if err != nil {
		return "", fmt.Errorf("resolve go toolchain (is go on PATH?): %w", err)
	}
	return filepath.Join(strings.TrimSpace(root.Stdout), "bin", "go"), nil
}
