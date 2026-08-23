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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/exec"
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
			return fmt.Errorf("scion source %q: %w", s.Source, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("scion source %q is not a directory", s.Source)
		}
	}
	return nil
}

// Resolve produces a host-local scion binary for goarch ("arm64"/"amd64").
// machine names the output file in os.TempDir so two jails on one host never
// share a build output.
//
// The Binary branch returns before any toolchain is touched, which is what
// lets the machine hosting the jail carry no Go, no module cache and no
// egress at all (issue #27). Callers run Validate first; Resolve does it
// again so it is safe on its own.
func Resolve(ctx context.Context, r exec.Runner, spec Spec, goarch, machine string) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	if spec.Binary != "" {
		if err := VerifyELFArch(spec.Binary, goarch); err != nil {
			return "", err
		}
		return spec.Binary, nil
	}

	goBin := "go"
	buildDir := spec.Source
	if spec.Version != "" {
		gb, dir, err := FetchModule(ctx, r, spec.Version)
		if err != nil {
			return "", err
		}
		goBin, buildDir = gb, dir
	}

	out := OutputPath(machine)
	if _, err := r.RunIn(ctx, buildDir, map[string]string{"GOOS": "linux", "GOARCH": goarch},
		goBin, "build", "-o", out, "./cmd/scion"); err != nil {
		return "", fmt.Errorf("cross-compile scion: %w", err)
	}
	return out, nil
}

// OutputPath is where Resolve writes a cross-compiled scion for machine.
func OutputPath(machine string) string {
	return filepath.Join(os.TempDir(), "lever-scion-"+machine)
}

// FetchModule downloads the pinned scion module via the Go module system and
// returns (goBinary, moduleSourceDir) for the cross-compile. It uses the REAL
// go binary (provision.GoBinary) for the build because the module cache lives
// outside any toolchain-manager project dir — a version manager that resolves
// `go` by walking up for a project file (asdf) cannot resolve it from the
// read-only module cache, whereas the absolute binary always works.
func FetchModule(ctx context.Context, r exec.Runner, version string) (goBin, dir string, err error) {
	goBin, err = provision.GoBinary(ctx, r)
	if err != nil {
		return "", "", err
	}
	out, err := r.Run(ctx, nil, goBin, "mod", "download", "-json", ModulePath+"@"+version)
	if err != nil {
		return "", "", fmt.Errorf("download scion %s: %w", version, err)
	}
	var dl struct{ Dir, Error string }
	if jerr := json.Unmarshal([]byte(out.Stdout), &dl); jerr != nil {
		return "", "", fmt.Errorf("parse `go mod download` output for scion %s: %w", version, jerr)
	}
	if dl.Error != "" {
		return "", "", fmt.Errorf("download scion %s: %s", version, dl.Error)
	}
	if dl.Dir == "" {
		return "", "", fmt.Errorf("`go mod download` returned no source dir for scion %s", version)
	}
	return goBin, dl.Dir, nil
}
