// Package guest provisions an Ubuntu jail guest — rootless container runtimes,
// the scion binary and its web assets, the remote-access login forwarder and
// scion's on-disk state — through host-side argv prefixes. It is shared by
// every backend that reaches its guest via a "run this as user X" prefix (orb,
// lima, ...); only the prefixes differ, the provisioning scripts don't.
//
// It owns the TRANSPORT and the IN-GUEST scripts only. Every artefact it
// installs is built elsewhere on the host and arrives as a local path:
// internal/provision/scionbin (the scion binary), internal/provision/webassets
// (the SPA) and internal/provision/loginfwd (the forwarder). Scion's file
// layout and settings keys come from internal/scion/layout; the scripts here
// are assembled from those constants rather than restating them.
package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

// Guest provisions an Ubuntu jail guest through host-side argv prefixes.
type Guest struct {
	Host       proc.Runner // host runner (builds, pipes)
	UserPrefix []string    // executes in-guest as the run user, e.g. ["orb","-m",m]
	RootPrefix []string    // executes in-guest as root, e.g. ["orb","-u","root","-m",m]
	Machine    string      // jail identifier (temp-file naming)
}

// RootRun / UserRun execute args inside the guest via the RootPrefix / UserPrefix
// transport respectively, e.g. RootPrefix ["orb","-u","root","-m",m] + args
// ["iptables","-S",chain] runs `orb -u root -m <m> iptables -S <chain>`. Both
// delegate to prefixRun, which defensively copies prefix[1:] before appending so
// concurrent callers can't alias/corrupt each other's argv (append may reuse the
// underlying array when capacity allows) — the single place the prefix-splat
// idiom lives.
func (g Guest) RootRun(ctx context.Context, args ...string) (proc.Result, error) {
	return g.prefixRun(ctx, g.RootPrefix, args...)
}

func (g Guest) UserRun(ctx context.Context, args ...string) (proc.Result, error) {
	return g.prefixRun(ctx, g.UserPrefix, args...)
}

func (g Guest) prefixRun(ctx context.Context, prefix []string, args ...string) (proc.Result, error) {
	argv := append(append([]string{}, prefix[1:]...), args...)
	return g.Host.Run(ctx, nil, prefix[0], argv...)
}

// pipeInto streams stdin into a bash script running inside the guest through
// prefix: `<prefix> bash -c <script>` with the host bytes on its stdin. It is
// how every host→guest byte stream travels (a binary, a settings file, an
// asset archive): argv only, through the Runner's stdin seam — no host shell,
// no `cat X | prefix bash -c '…'` pipeline, no quoting of the prefix words.
// bash resolves on the prefix user's PATH (root's for RootPrefix, which no run
// user can write to).
func (g Guest) pipeInto(ctx context.Context, prefix []string, stdin io.Reader, script string) error {
	argv := append(append([]string{}, prefix[1:]...), "bash", "-c", script)
	_, err := g.Host.RunStdin(ctx, stdin, nil, prefix[0], argv...)
	return err
}

// EnsureRuntimes installs prereqs + rootless Docker and rootless Podman.
// Idempotent: the rootless install script and systemctl --start are safe to re-run.
// Podman is daemonless so no service startup is needed; scion auto-prefers it over Docker.
func (g Guest) EnsureRuntimes(ctx context.Context, runUser string) error {
	root := func(script string) error {
		_, err := g.RootRun(ctx, "bash", "-lc", script)
		return err
	}
	user := func(script string) error {
		_, err := g.UserRun(ctx, "bash", "-lc", script)
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
	res, err := g.UserRun(ctx, "uname", "-m")
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

// scionDestPath is where scion is installed in the guest.
const scionDestPath = "/usr/local/bin/scion"

// ScionSpec names the one place lever should get scion from. The type is
// scionbin's; the alias keeps the backends' `guest.ScionSpec{...}` literals
// beside the other guest calls they make.
type ScionSpec = scionbin.Spec

// EnsureScion puts the configured scion into the guest at scionDestPath, plus
// its web assets when the spec asks for them. The binary is resolved on the
// host by scionbin.Resolve (the Binary branch never touches a toolchain, which
// is what lets the jail host carry no Go — issue #27) and streamed in by
// InstallRootBinaryIfChanged.
func (g Guest) EnsureScion(ctx context.Context, spec ScionSpec) error {
	// Validate the spec BEFORE touching the guest, so a plainly wrong config
	// fails without a round-trip and without depending on the guest being up.
	if err := spec.Validate(); err != nil {
		return err
	}
	arch, err := g.GOARCH(ctx)
	if err != nil {
		return fmt.Errorf("detect guest architecture: %w", err)
	}
	bin, err := scionbin.Resolve(ctx, g.Host, spec, arch, g.Machine)
	if err != nil {
		return err
	}
	if _, err := g.InstallRootBinaryIfChanged(ctx, bin, scionDestPath); err != nil {
		return err
	}
	return g.EnsureScionWebAssets(ctx, spec)
}

// InstallRootBinary streams a host-local executable into the guest at destPath
// (mode +x), owned by root, via the RootPrefix transport. It is the backend-
// agnostic way to place a binary the guest needs — scion (above) and the
// acceptance gate's lever-agent both use it, so the "which prefix reaches the
// guest as root" knowledge lives only in the backend's RootPrefix, never in a
// caller.
//
// The install is atomic: the host file is the stdin of a guest-side script
// that writes a temp path, checks its byte count against the host file's,
// makes it executable, then mv's it over destPath (mv is atomic on the same
// filesystem), so a mid-stream failure can't leave a truncated, executable
// binary at destPath. The count check is what makes that true: a stream that
// ends early is a plain EOF to `cat`, which exits 0, so without it the `&&`
// chain would install the truncated file. destPath is a fixed literal at
// every call site today (not attacker input), but it — and its derived .tmp
// — are still shell-quoted, because they are interpolated into the guest
// script.
func (g Guest) InstallRootBinary(ctx context.Context, localPath, destPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	if err := g.pipeInto(ctx, g.RootPrefix, f, installRootBinaryScript(destPath, fi.Size())); err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	return nil
}

// installRootBinaryScript is the guest-side half of InstallRootBinary: read
// stdin into destPath.tmp, refuse it unless exactly size bytes arrived, then
// swap it in. `wc -c <` rather than `stat` because the two stats disagree
// across GNU and BSD and the injection test runs this script on the host.
// Split out so a test can pin it.
func installRootBinaryScript(destPath string, size int64) string {
	tmp := shellSingleQuote(destPath + ".tmp")
	return fmt.Sprintf("cat > %s && [ \"$(wc -c < %s)\" -eq %d ] && chmod +x %s && mv %s %s", tmp, tmp, size, tmp, tmp, shellSingleQuote(destPath))
}

// guestFileDigest returns the hex sha256 the guest reports for path, or ""
// when it cannot be read (no such file, no sha256sum, an unreadable path).
//
// Absolute path, not a bare name: UserRun passes no env, so a bare name
// resolves on the guest user's PATH, which precedes /usr/bin with
// run-user-writable directories. A shim there could report the expected
// digest for a replaced binary and pin it forever. That needs guest root
// (who could replace the binary outright), but the fix is one word.
func (g Guest) guestFileDigest(ctx context.Context, path string) string {
	res, err := g.UserRun(ctx, "/usr/bin/sha256sum", path)
	if err != nil {
		return ""
	}
	// `sha256sum` prints "<hex>  <path>"; take the digest field only.
	if f := strings.Fields(res.Stdout); len(f) > 0 {
		return f[0]
	}
	return ""
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
// already holds exactly those bytes, and reports whether it installed.
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
func (g Guest) InstallRootBinaryIfChanged(ctx context.Context, localPath, destPath string) (bool, error) {
	want, err := hashFile(localPath)
	if err != nil {
		return false, fmt.Errorf("hashing %s: %w", localPath, err)
	}
	if g.guestFileDigest(ctx, destPath) == want {
		return false, nil
	}
	if err := g.InstallRootBinary(ctx, localPath, destPath); err != nil {
		return false, err
	}
	return true, nil
}

// shellSingleQuote wraps s in single quotes safe for POSIX shells, escaping any
// embedded single quote as the standard '\” sequence (close quote, escaped
// quote, reopen quote).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
