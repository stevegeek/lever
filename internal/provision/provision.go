// Package provision holds the host-side build pipelines that produce the
// artefacts lever puts into a jail: the scion binary (scionbin) and scion's
// web UI (webassets). Each
// subpackage takes a proc.Runner and returns a local path; none of them knows
// how to reach a guest (that is internal/backend/guest's job). This root
// package carries only what the subpackages share.
package provision

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// GoBinary resolves the real go binary (GOROOT/bin/go) rather than trusting
// `go` on PATH: a version manager that resolves `go` by walking up for a
// project file (asdf, mise) fails outside any project, which is where the
// module cache and the temp build directories live.
func GoBinary(ctx context.Context, r proc.Runner) (string, error) {
	root, err := r.Run(ctx, nil, "go", "env", "GOROOT")
	if err != nil {
		return "", fmt.Errorf("resolve go toolchain (is go on PATH?): %w", err)
	}
	return filepath.Join(strings.TrimSpace(root.Stdout), "bin", "go"), nil
}
