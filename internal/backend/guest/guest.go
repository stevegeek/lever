// Package guest provisions an Ubuntu jail guest — rootless container runtimes
// plus a cross-compiled scion binary — through host-side argv prefixes. It is
// shared by every backend that reaches its guest via a "run this as user X"
// prefix (orb, lima, ...); only the prefixes differ, the provisioning scripts
// don't.
package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/exec"
)

// Guest provisions an Ubuntu jail guest through host-side argv prefixes.
type Guest struct {
	Host       exec.Runner // host runner (builds, pipes)
	UserPrefix []string    // executes in-guest as the run user, e.g. ["orb","-m",m]
	RootPrefix []string    // executes in-guest as root, e.g. ["orb","-u","root","-m",m]
	Machine    string      // jail identifier (temp-file naming)
}

// rootRun / userRun execute args inside the guest via the RootPrefix / UserPrefix
// transport respectively, e.g. RootPrefix ["orb","-u","root","-m",m] + args
// ["iptables","-S",chain] runs `orb -u root -m <m> iptables -S <chain>`. Both
// delegate to prefixRun, which defensively copies prefix[1:] before appending so
// concurrent callers can't alias/corrupt each other's argv (append may reuse the
// underlying array when capacity allows) — the single place the prefix-splat
// idiom lives.
func (g Guest) rootRun(ctx context.Context, args ...string) (exec.Result, error) {
	return g.prefixRun(ctx, g.RootPrefix, args...)
}

func (g Guest) userRun(ctx context.Context, args ...string) (exec.Result, error) {
	return g.prefixRun(ctx, g.UserPrefix, args...)
}

func (g Guest) prefixRun(ctx context.Context, prefix []string, args ...string) (exec.Result, error) {
	argv := append(append([]string{}, prefix[1:]...), args...)
	return g.Host.Run(ctx, nil, prefix[0], argv...)
}

// EnsureRuntimes installs prereqs + rootless Docker and rootless Podman.
// Idempotent: the rootless install script and systemctl --start are safe to re-run.
// Podman is daemonless so no service startup is needed; scion auto-prefers it over Docker.
func (g Guest) EnsureRuntimes(ctx context.Context, runUser string) error {
	root := func(script string) error {
		_, err := g.rootRun(ctx, "bash", "-lc", script)
		return err
	}
	user := func(script string) error {
		_, err := g.userRun(ctx, "bash", "-lc", script)
		return err
	}
	// Guard the apt step behind a dpkg presence check so a re-apply (or a second
	// egress posture on the same VM) does NOT re-run apt. This is not just an
	// optimisation: once lever's egress chain is active it drops the RFC1918
	// ranges, which on Lima include the guest's own DNS upstream (systemd-resolved
	// forwards to a 192.168.x address), so `apt-get update` can no longer resolve
	// the mirrors and hangs. The first EnsureUp (fresh VM, no chain yet) installs
	// everything; subsequent ones find the packages present and skip apt entirely,
	// needing no guest DNS. `dpkg -s <pkgs>` succeeds iff ALL are installed.
	//
	// curl is how lever's own hub calls run inside the jail (internal/hubapi/
	// jailcurl.go); netcat-openbsd is how the remote-access proxy dials the hub
	// through the jail (internal/remoteproxy/jaildial.go). Both are in the
	// Debian base image today, so naming them here changes nothing on an
	// existing guest — the guard still passes and apt still never runs. They
	// are named so a future base image cannot drop one and break a lever
	// feature silently.
	if err := root(`dpkg -s uidmap dbus-user-session fuse-overlayfs slirp4netns curl netcat-openbsd iptables podman >/dev/null 2>&1 || { DEBIAN_FRONTEND=noninteractive apt-get update -qq && apt-get install -y -qq uidmap dbus-user-session fuse-overlayfs slirp4netns curl netcat-openbsd iptables podman; }`); err != nil {
		return fmt.Errorf("apt prereqs: %w", err)
	}
	// Ubuntu >= 23.10 (the Lima jail guest is 24.04) ships
	// kernel.apparmor_restrict_unprivileged_userns=1, which blocks the rootless
	// runtimes' rootlesskit/pasta from creating the unprivileged user namespace
	// they require — without this the rootless Docker/Podman install fails with
	// "fork/exec /proc/self/exe: permission denied". The jail is a dedicated VM
	// whose OWN kernel is the containment boundary (backend Guarantee 0:
	// separate-kernel), and its sole purpose is to run the agent's untrusted
	// rootless containers, so relaxing this in-guest knob is the intended jail
	// posture — it is scoped to the throwaway guest kernel and does not touch the
	// host's. Persisted so it survives a guest reboot and applied live for this
	// boot. Tolerant of guests without the knob (e.g. the OrbStack distro), where
	// the sysctl key simply doesn't exist.
	if err := root(`printf 'kernel.apparmor_restrict_unprivileged_userns=0\n' > /etc/sysctl.d/99-lever-userns.conf; sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 2>/dev/null || true`); err != nil {
		return fmt.Errorf("relax unprivileged userns for rootless runtimes: %w", err)
	}
	// Enable lingering so the run user's systemd instance (and thus rootless
	// dockerd) survives after the provisioning SSH session closes. `loginctl
	// enable-linger` is the canonical path (used on OrbStack), but on the Lima
	// Ubuntu guest systemd-logind's D-Bus interface intermittently hangs — every
	// loginctl call then blocks until timeout and the step fails ("Could not
	// enable linger: Connection timed out"). enable-linger merely creates
	// /var/lib/systemd/linger/<user>, so fall back to writing that marker directly
	// (the documented equivalent) when loginctl is unresponsive: try it under a
	// short timeout, else touch the marker. Deterministic on both backends.
	if err := root(fmt.Sprintf(`grep -q '^%s:' /etc/subuid || echo '%s:100000:65536' >> /etc/subuid; grep -q '^%s:' /etc/subgid || echo '%s:100000:65536' >> /etc/subgid; timeout 8 loginctl enable-linger %s 2>/dev/null || { mkdir -p /var/lib/systemd/linger && : > /var/lib/systemd/linger/%s; }`,
		runUser, runUser, runUser, runUser, runUser, runUser)); err != nil {
		return fmt.Errorf("subid/linger: %w", err)
	}
	if err := user(`command -v dockerd-rootless.sh >/dev/null 2>&1 || curl -fsSL https://get.docker.com/rootless | sh`); err != nil {
		return fmt.Errorf("rootless install: %w", err)
	}
	if err := user(`export XDG_RUNTIME_DIR=/run/user/$(id -u); export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/docker.sock; systemctl --user enable --now docker 2>/dev/null || (nohup dockerd-rootless.sh >/tmp/lever-dockerd.log 2>&1 &); timeout 30 sh -c 'until docker info >/dev/null 2>&1; do sleep 1; done'`); err != nil {
		return fmt.Errorf("start rootless dockerd: %w", err)
	}
	// Per-agent network isolation: agents run in their own pasta netns (no
	// --network=host; see jail.jailEnvFor), which makes each container's 127.0.0.1
	// private and isolates the in-container gateway proxy from co-resident agents.
	// The one thing host-net was buying was the agent's reach to the VM-loopback
	// hub. Restore it with pasta --map-host-loopback on 169.254.1.2 — the address
	// podman already resolves host.containers.internal to — so scion's
	// auto-computed container hub endpoint (host.containers.internal:PORT) reaches
	// the VM-loopback hub. pasta_options APPEND to podman's defaults (dns-forward,
	// map-guest-addr preserved). A containers.conf.d drop-in so we never clobber a
	// base containers.conf; idempotent (deterministic content).
	if err := user(`mkdir -p ~/.config/containers/containers.conf.d && cat > ~/.config/containers/containers.conf.d/10-lever-pasta.conf <<'EOF'
[network]
pasta_options = ["--map-host-loopback", "169.254.1.2"]
EOF`); err != nil {
		return fmt.Errorf("stage pasta host-loopback mapping: %w", err)
	}
	return nil
}

// GOARCH returns the guest's Go cross-compile arch, detected via `uname -m`
// run inside the guest (as the run user).
func (g Guest) GOARCH(ctx context.Context) (string, error) {
	res, err := g.userRun(ctx, "uname", "-m")
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

// scionDestPath is where scion is installed in the guest.
const scionDestPath = "/usr/local/bin/scion"

// ScionSpec names the one place lever should get scion from. At most one of
// Binary/Source/Version is set; config validation enforces that
// (internal/config). A struct rather than positional strings because three
// same-typed parameters across two call sites is a mis-ordering waiting to
// happen.
type ScionSpec struct {
	// Binary is a host-local, already-built linux binary, installed as-is. No
	// Go toolchain, module cache or egress is needed on this host.
	Binary string
	// Source is a host checkout to cross-compile.
	Source string
	// Version pins a scion module version/commit to fetch and cross-compile.
	Version string
	// WebUI additionally builds scion's SPA on the host and stages it into the
	// guest, so the hub can serve the web UI (see webassets.go). A field on the
	// spec rather than a parameter on EnsureScion so the two backends' copies of
	// the provisioning block stay one struct literal: that block has drifted
	// before — Binary was added to both literals while the guard around them was
	// updated in neither (see backend.Config.HasScion).
	WebUI bool
}

// EnsureScion puts the configured scion into the guest at scionDestPath, plus
// its web assets when the spec asks for them.
func (g Guest) EnsureScion(ctx context.Context, spec ScionSpec) error {
	bin, err := g.resolveScionBinary(ctx, spec)
	if err != nil {
		return err
	}
	if err := g.InstallRootBinaryIfChanged(ctx, bin, scionDestPath); err != nil {
		return err
	}
	return g.EnsureScionWebAssets(ctx, spec)
}

// resolveScionBinary produces a host-local scion binary for the guest's
// architecture.
//
// It is the ONLY place that knows about Go. The Binary branch returns before
// any toolchain is touched, which is what lets the machine hosting the jail
// carry no Go, no module cache and no egress at all (issue #27).
func (g Guest) resolveScionBinary(ctx context.Context, spec ScionSpec) (string, error) {
	// Validate the spec BEFORE touching the guest, so a plainly wrong config
	// fails without a round-trip and without depending on the guest being up.
	if spec.Binary == "" && spec.Source == "" && spec.Version == "" {
		return "", fmt.Errorf("no scion configured: set one of scion.binary, scion.source or scion.version")
	}
	if spec.Source != "" {
		fi, err := os.Stat(spec.Source)
		if err != nil {
			return "", fmt.Errorf("scion source %q: %w", spec.Source, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("scion source %q is not a directory", spec.Source)
		}
	}

	arch, err := g.GOARCH(ctx)
	if err != nil {
		return "", fmt.Errorf("detect guest architecture: %w", err)
	}
	if spec.Binary != "" {
		if err := verifyELFArch(spec.Binary, arch); err != nil {
			return "", err
		}
		return spec.Binary, nil
	}

	goBin := "go"
	buildDir := spec.Source
	if spec.Version != "" {
		gb, dir, err := g.fetchScionModule(ctx, spec.Version)
		if err != nil {
			return "", err
		}
		goBin, buildDir = gb, dir
	}

	out := filepath.Join(os.TempDir(), "lever-scion-"+g.Machine)
	if _, err := g.Host.RunIn(ctx, buildDir, map[string]string{"GOOS": "linux", "GOARCH": arch},
		goBin, "build", "-o", out, "./cmd/scion"); err != nil {
		return "", fmt.Errorf("cross-compile scion: %w", err)
	}
	return out, nil
}

// InstallRootBinary streams a host-local executable into the guest at destPath
// (mode +x), owned by root, via the RootPrefix transport. It is the backend-
// agnostic way to place a binary the guest needs — scion (above) and the
// acceptance gate's lever-agent both use it, so the "which prefix reaches the
// guest as root" knowledge lives only in the backend's RootPrefix, never in a
// caller.
//
// The install is atomic: it pipes the host file to a temp path in the guest,
// makes it executable, then mv's it over destPath (mv is atomic on the same
// filesystem), so a mid-stream failure can't leave a truncated, executable
// binary at destPath. `set -o pipefail` propagates a left-side (host `cat`)
// failure instead of letting the successful right side mask it. bash (not sh)
// is required for pipefail. destPath is a fixed literal at every call site
// today (not attacker input), but it — and its derived .tmp — are still
// shell-quoted: the nested `bash -c '<inner script>'` argument is itself
// embedded inside the OUTER install script, so a raw destPath containing a
// single quote would close that quoting from the OUTER bash's perspective and
// let anything after it run as an extra host-side command. Quoting closes
// that off for any future caller with a dynamic destPath.
func (g Guest) InstallRootBinary(ctx context.Context, localPath, destPath string) error {
	rootWords := make([]string, 0, len(g.RootPrefix))
	for _, w := range g.RootPrefix {
		rootWords = append(rootWords, shellSingleQuote(w))
	}
	tmp := destPath + ".tmp"
	inner := fmt.Sprintf("cat > %s && chmod +x %s && mv %s %s",
		shellSingleQuote(tmp), shellSingleQuote(tmp), shellSingleQuote(tmp), shellSingleQuote(destPath))
	install := fmt.Sprintf(
		`set -o pipefail; cat %s | %s bash -c %s`,
		shellSingleQuote(localPath), strings.Join(rootWords, " "), shellSingleQuote(inner))
	if _, err := g.Host.Run(ctx, nil, "bash", "-c", install); err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	return nil
}

// hashFile returns the hex sha256 of a host-local file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InstallRootBinaryIfChanged installs localPath at destPath unless the guest
// already holds exactly those bytes.
//
// scion is 158MB and was otherwise re-streamed into the guest on every
// bring-up, in every mode, whether or not it had changed.
//
// It compares against the GUEST BINARY ITSELF, by hashing it in place — not
// against a marker file recording what lever installed once. A marker attests a
// past event; only the file answers "is the right binary there NOW?". Hashing
// the file covers deletion, truncation and out-of-band replacement in one
// check, keeps `lever up` self-healing the way it was before any skip existed,
// and needs no second write whose ordering has to be defended.
//
// It costs a 158MB read off guest disk rather than a 158MB stream across the
// transport, which is the cheaper side of that trade by a wide margin.
//
// Fails open: if the guest digest cannot be read for any reason — no such file,
// no sha256sum, an unreadable path — lever installs. Being wrong that way costs
// redundant work; the other way would skip a real install.
func (g Guest) InstallRootBinaryIfChanged(ctx context.Context, localPath, destPath string) error {
	want, err := hashFile(localPath)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", localPath, err)
	}
	// Absolute path, not a bare name: userRun passes no env, so a bare name
	// resolves on the guest user's PATH, which precedes /usr/bin with
	// run-user-writable directories. A shim there could report the expected
	// digest for a replaced binary and pin it forever. That needs guest root
	// (who could replace scion outright), but the fix is one word.
	if res, err := g.userRun(ctx, "/usr/bin/sha256sum", destPath); err == nil {
		// `sha256sum` prints "<hex>  <path>"; take the digest field only.
		if f := strings.Fields(res.Stdout); len(f) > 0 && f[0] == want {
			return nil
		}
	}
	return g.InstallRootBinary(ctx, localPath, destPath)
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
