// Package guest provisions an Ubuntu jail guest — rootless container runtimes
// plus a cross-compiled scion binary — through host-side argv prefixes. It is
// shared by every backend that reaches its guest via a "run this as user X"
// prefix (orb, lima, ...); only the prefixes differ, the provisioning scripts
// don't.
package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lever-to/lever/internal/exec"
)

// Guest provisions an Ubuntu jail guest through host-side argv prefixes.
type Guest struct {
	Host       exec.Runner // host runner (builds, pipes)
	UserPrefix []string    // executes in-guest as the run user, e.g. ["orb","-m",m]
	RootPrefix []string    // executes in-guest as root, e.g. ["orb","-u","root","-m",m]
	Machine    string      // jail identifier (temp-file naming)
}

// EnsureRuntimes installs prereqs + rootless Docker and rootless Podman.
// Idempotent: the rootless install script and systemctl --start are safe to re-run.
// Podman is daemonless so no service startup is needed; scion auto-prefers it over Docker.
func (g Guest) EnsureRuntimes(ctx context.Context, runUser string) error {
	root := func(script string) error {
		_, err := g.Host.Run(ctx, nil, g.RootPrefix[0], append(append([]string{}, g.RootPrefix[1:]...), "bash", "-lc", script)...)
		return err
	}
	user := func(script string) error {
		_, err := g.Host.Run(ctx, nil, g.UserPrefix[0], append(append([]string{}, g.UserPrefix[1:]...), "bash", "-lc", script)...)
		return err
	}
	if err := root(`DEBIAN_FRONTEND=noninteractive apt-get update -qq && apt-get install -y -qq uidmap dbus-user-session fuse-overlayfs slirp4netns curl iptables podman`); err != nil {
		return fmt.Errorf("apt prereqs: %w", err)
	}
	if err := root(fmt.Sprintf(`grep -q '^%s:' /etc/subuid || echo '%s:100000:65536' >> /etc/subuid; grep -q '^%s:' /etc/subgid || echo '%s:100000:65536' >> /etc/subgid; loginctl enable-linger %s`,
		runUser, runUser, runUser, runUser, runUser)); err != nil {
		return fmt.Errorf("subid/linger: %w", err)
	}
	if err := user(`command -v dockerd-rootless.sh >/dev/null 2>&1 || curl -fsSL https://get.docker.com/rootless | sh`); err != nil {
		return fmt.Errorf("rootless install: %w", err)
	}
	if err := user(`export XDG_RUNTIME_DIR=/run/user/$(id -u); export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/docker.sock; systemctl --user enable --now docker 2>/dev/null || (nohup dockerd-rootless.sh >/tmp/lever-dockerd.log 2>&1 &); timeout 30 sh -c 'until docker info >/dev/null 2>&1; do sleep 1; done'`); err != nil {
		return fmt.Errorf("start rootless dockerd: %w", err)
	}
	return nil
}

// GOARCH returns the guest's Go cross-compile arch, detected via `uname -m`
// run inside the guest (as the run user).
func (g Guest) GOARCH(ctx context.Context) (string, error) {
	res, err := g.Host.Run(ctx, nil, g.UserPrefix[0], append(g.UserPrefix[1:], "uname", "-m")...)
	if err != nil {
		return "", fmt.Errorf("uname -m: %w", err)
	}
	switch u := strings.TrimSpace(res.Stdout); u {
	case "aarch64", "arm64":
		return "arm64", nil
	case "x86_64", "amd64":
		return "amd64", nil
	default:
		return "", fmt.Errorf("unrecognized guest architecture %q", u)
	}
}

// ensureScion cross-compiles scion from a host source checkout for linux/arm64
// and installs it into the jail at /usr/local/bin/scion. The build runs on the
// HOST (Go's build cache makes re-runs incremental, so this is cheap to repeat).
// The binary is piped into the jail via `bash -c "cat <bin> | orb … bash -c 'cat
// > … .tmp && chmod && mv'"` because the Runner has no stdin channel. The install
// is atomic: it writes a temp file then mv's it over the destination (mv is
// atomic on the same filesystem), so a mid-stream failure can't leave a
// truncated, executable /usr/local/bin/scion. `set -o pipefail` makes a
// left-side failure (e.g. the host `cat`) propagate instead of being masked by a
// successful right side. bash (not sh) is required because dash on Linux hosts —
// where the linux-docker backend will run — does not support `set -o pipefail`.
// scionModulePath is the upstream scion Go module. `version` ("" → source mode)
// pins a commit/tag fetched via the Go module system.
const scionModulePath = "github.com/GoogleCloudPlatform/scion"

func (g Guest) EnsureScion(ctx context.Context, source, version string) error {
	goBin := "go"
	buildDir := source
	if version != "" {
		gb, dir, err := g.fetchScionModule(ctx, version)
		if err != nil {
			return err
		}
		goBin, buildDir = gb, dir
	} else {
		fi, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("scion source %q: %w", source, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("scion source %q is not a directory", source)
		}
	}
	bin := filepath.Join(os.TempDir(), "lever-scion-"+g.Machine)
	arch, err := g.GOARCH(ctx)
	if err != nil {
		return fmt.Errorf("detect guest architecture: %w", err)
	}
	if _, err := g.Host.RunIn(ctx, buildDir, map[string]string{"GOOS": "linux", "GOARCH": arch},
		goBin, "build", "-o", bin, "./cmd/scion"); err != nil {
		return fmt.Errorf("cross-compile scion: %w", err)
	}
	rootWords := make([]string, 0, len(g.RootPrefix))
	for _, w := range g.RootPrefix {
		rootWords = append(rootWords, shellSingleQuote(w))
	}
	install := fmt.Sprintf(
		`set -o pipefail; cat %s | %s bash -c 'cat > /usr/local/bin/scion.tmp && chmod +x /usr/local/bin/scion.tmp && mv /usr/local/bin/scion.tmp /usr/local/bin/scion'`,
		shellSingleQuote(bin), strings.Join(rootWords, " "))
	if _, err := g.Host.Run(ctx, nil, "bash", "-c", install); err != nil {
		return fmt.Errorf("install scion into jail: %w", err)
	}
	return nil
}

// fetchScionModule downloads the pinned scion module via the Go module system
// and returns (goBinary, moduleSourceDir) for the cross-compile. It resolves the
// REAL go binary (GOROOT/bin/go) and uses it for the build because the module
// cache lives outside any toolchain-manager project dir — e.g. a version manager
// that resolves `go` by walking up for a project file (asdf) cannot resolve it
// from the read-only module cache, whereas the absolute binary always works.
func (g Guest) fetchScionModule(ctx context.Context, version string) (goBin, dir string, err error) {
	root, err := g.Host.Run(ctx, nil, "go", "env", "GOROOT")
	if err != nil {
		return "", "", fmt.Errorf("resolve go toolchain (is go on PATH?): %w", err)
	}
	goBin = filepath.Join(strings.TrimSpace(root.Stdout), "bin", "go")
	out, err := g.Host.Run(ctx, nil, goBin, "mod", "download", "-json", scionModulePath+"@"+version)
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

// shellSingleQuote wraps s in single quotes safe for POSIX shells, escaping any
// embedded single quote as the standard '\” sequence (close quote, escaped
// quote, reopen quote).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
