// Package guest provisions an Ubuntu jail guest (rootless container runtimes,
// the scion binary and its web assets, the remote-access login forwarder and
// scion's on-disk state) through host-side argv prefixes, for every backend
// that reaches its guest via a "run this as user X" prefix.
//
// It owns the transport and the in-guest scripts only. Every artefact it
// installs is built elsewhere on the host and arrives as a local path. Scion's
// file layout and settings keys come from internal/scion/layout.
package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/imageref"
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

// RootRun / UserRun execute args inside the guest via the RootPrefix /
// UserPrefix transport respectively.
func (g Guest) RootRun(ctx context.Context, args ...string) (proc.Result, error) {
	return g.prefixRun(ctx, g.RootPrefix, args...)
}

func (g Guest) UserRun(ctx context.Context, args ...string) (proc.Result, error) {
	return g.prefixRun(ctx, g.UserPrefix, args...)
}

// prefixRun copies prefix[1:] before appending so concurrent callers cannot
// alias each other's argv through a shared backing array.
func (g Guest) prefixRun(ctx context.Context, prefix []string, args ...string) (proc.Result, error) {
	argv := append(append([]string{}, prefix[1:]...), args...)
	return g.Host.Run(LimitOutput(ctx), nil, prefix[0], argv...)
}

// OutputLimit caps each of stdout and stderr of a guest command. Guest root
// writes that output, and the host holds all of it in memory, so a hostile
// guest could otherwise stream without end. Real output is far below it.
const OutputLimit = 8 << 20

// LimitOutput returns ctx with OutputLimit on every command run under it,
// keeping a smaller cap ctx already carries. A command past it is killed and
// fails with proc.ErrOutputLimit.
func LimitOutput(ctx context.Context) context.Context {
	if n, ok := proc.OutputLimit(ctx); ok && n <= OutputLimit {
		return ctx
	}
	return proc.WithOutputLimit(ctx, OutputLimit)
}

// stderrMax bounds the guest stderr an error quotes.
const stderrMax = 500

// quoteStderr is guest stderr as one bounded, printable line for an error.
func quoteStderr(res proc.Result) string {
	return SafeLine(strings.TrimSpace(res.Stderr), stderrMax)
}

// runUserRE admits the names a Lima or OrbStack guest user can have (a
// lowercase POSIX name, or a macOS short name with capitals or dots) and
// nothing a shell, a path or /etc/subuid treats specially: the run user is
// written into root scripts, file names and transport argv.
var runUserRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)

// ValidRunUser refuses a run user name or uid the guest reported in any form
// other than a plain user name and a canonical decimal uid, and refuses root.
func ValidRunUser(user, uid string) error {
	if !runUserRE.MatchString(user) {
		return fmt.Errorf("the guest reported run user %q, which is not a plain user name", SafeLine(user, 64))
	}
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || strconv.FormatUint(n, 10) != uid || n == math.MaxUint32 {
		return fmt.Errorf("the guest reported run uid %q, which is not a decimal uid", SafeLine(uid, 64))
	}
	if n == 0 {
		return fmt.Errorf("the guest reported run user %s with uid 0; lever does not run the jail as root", user)
	}
	return nil
}

// pipeInto streams stdin into `<prefix> bash -c <script>`. Every host→guest
// byte stream must use it: argv only, no host shell, no quoting of the prefix
// words. bash resolves on the prefix user's PATH (root's for RootPrefix, which
// no run user can write to).
func (g Guest) pipeInto(ctx context.Context, prefix []string, stdin io.Reader, script string) error {
	argv := append(append([]string{}, prefix[1:]...), "bash", "-c", script)
	_, err := g.Host.RunStdin(LimitOutput(ctx), stdin, nil, prefix[0], argv...)
	return err
}

// EnsureRuntimes installs the prereqs and rootless Podman, which scion runs
// the agents in. Idempotent. Podman is daemonless, so nothing is started.
func (g Guest) EnsureRuntimes(ctx context.Context, runUser string) error {
	root := func(script string) error {
		_, err := g.RootRun(ctx, "bash", "-lc", script)
		return err
	}
	user := func(script string) error {
		_, err := g.UserRun(ctx, "bash", "-lc", script)
		return err
	}
	// The dpkg guard is not just an optimisation: once the egress chain is
	// active, the closed posture drops the guest's own DNS upstream (an RFC1918
	// address on Lima), so `apt-get update` would hang. A fresh VM has no chain
	// yet and installs everything; later runs find the packages and skip apt.
	//
	// curl (in-jail hub calls), netcat-openbsd (the remote-access proxy's jail
	// dial) and passt (only a Recommends of Ubuntu's podman; agents need it to
	// reach the hub) are named so a future base image cannot silently drop one.
	if err := root(`dpkg -s uidmap dbus-user-session fuse-overlayfs slirp4netns passt curl netcat-openbsd iptables podman >/dev/null 2>&1 || { DEBIAN_FRONTEND=noninteractive apt-get update -qq && apt-get install -y -qq uidmap dbus-user-session fuse-overlayfs slirp4netns passt curl netcat-openbsd iptables podman; }`); err != nil {
		return fmt.Errorf("apt prereqs: %w", err)
	}
	// Ubuntu >= 23.10 sets kernel.apparmor_restrict_unprivileged_userns=1,
	// which makes rootless podman fail with "fork/exec /proc/self/exe:
	// permission denied". Relaxing it is the intended jail posture: it is
	// scoped to the guest kernel, which is itself the containment boundary,
	// and never touches the host's. Tolerant of guests without the knob.
	if err := root(`printf 'kernel.apparmor_restrict_unprivileged_userns=0\n' > /etc/sysctl.d/99-lever-userns.conf; sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 2>/dev/null || true`); err != nil {
		return fmt.Errorf("relax unprivileged userns for rootless runtimes: %w", err)
	}
	// Lingering keeps the run user's systemd instance and runtime dir alive
	// after the provisioning session closes. On the Lima guest systemd-logind's
	// D-Bus interface intermittently hangs, so loginctl gets a short timeout
	// and the fallback writes /var/lib/systemd/linger/<user> directly, the
	// documented equivalent.
	if err := root(fmt.Sprintf(`grep -q '^%s:' /etc/subuid || echo '%s:100000:65536' >> /etc/subuid; grep -q '^%s:' /etc/subgid || echo '%s:100000:65536' >> /etc/subgid; timeout 8 loginctl enable-linger %s 2>/dev/null || { mkdir -p /var/lib/systemd/linger && : > /var/lib/systemd/linger/%s; }`,
		runUser, runUser, runUser, runUser, runUser, runUser)); err != nil {
		return fmt.Errorf("subid/linger: %w", err)
	}
	// A guest may still run an unused rootless dockerd; stop and disable it.
	// A no-op once it is off or was never installed.
	if err := user(dockerOffScript); err != nil {
		return fmt.Errorf("disable the unused rootless dockerd: %w", err)
	}
	// Agents run in their own pasta netns and reach the VM-loopback hub at
	// PastaHostAddr.
	if err := user(pastaDropInScript()); err != nil {
		return fmt.Errorf("stage pasta hub mapping: %w", err)
	}
	if err := user(agentRegistryBlockScript); err != nil {
		return fmt.Errorf("block pulls from %s: %w", imageref.Registry, err)
	}
	return nil
}

// agentRegistryBlockScript makes podman refuse any pull from lever's agent
// image registry name. Agents run images load-image put in the store, and
// no registry by that name exists; a pull happens only when the image is
// missing, and then it must fail rather than ask whatever answers for that
// name on the network. Loading, tagging and running stored images do not
// consult it.
const agentRegistryBlockScript = `set -e
mkdir -p "$HOME/.config/containers/registries.conf.d"
cat > "$HOME/.config/containers/registries.conf.d/10-lever-agent-images.conf" <<'LEVER_EOF'
[[registry]]
location = "` + imageref.Registry + `"
blocked = true
LEVER_EOF
`

// dockerOffScript exits 0 either way. A stale pid file is only acted on while
// that pid is still a dockerd, so a reused pid is never killed. The pid file
// lives in the run user's runtime dir, which no agent container sees.
const dockerOffScript = `export XDG_RUNTIME_DIR=/run/user/$(id -u)
if timeout 10 systemctl --user -q is-enabled docker 2>/dev/null || timeout 10 systemctl --user -q is-active docker 2>/dev/null; then
  timeout 30 systemctl --user disable --now docker >/dev/null 2>&1 || true
fi
pid=$(cat "$XDG_RUNTIME_DIR/docker.pid" 2>/dev/null) || pid=
case "$pid" in ''|*[!0-9]*) ;; *)
  [ "$(cat /proc/$pid/comm 2>/dev/null)" = dockerd ] && kill "$pid" 2>/dev/null ;;
esac
true`

// GOARCH returns the guest's Go cross-compile arch.
func (g Guest) GOARCH(ctx context.Context) (string, error) {
	res, err := g.UserRun(ctx, "uname", "-m")
	if err != nil {
		return "", fmt.Errorf("uname -m: %w", err)
	}
	return goarchOf(res.Stdout)
}

func goarchOf(uname string) (string, error) {
	switch u := strings.TrimSpace(uname); u {
	case "aarch64", "arm64":
		return "arm64", nil
	case "x86_64", "amd64":
		return "amd64", nil
	default:
		return "", fmt.Errorf("unrecognized guest architecture %q", SafeLine(u, 64))
	}
}

const scionDestPath = "/usr/local/bin/scion"

// ScionSpec names the one place lever should get scion from.
type ScionSpec = scionbin.Spec

// EnsureScion puts the configured scion into the guest at scionDestPath, plus
// its web assets when the spec asks for them.
func (g Guest) EnsureScion(ctx context.Context, spec ScionSpec) error {
	return g.EnsureScionFrom(ctx, spec, nil)
}

// EnsureScionFrom is EnsureScion with the guest's arch and the installed
// binary's digest taken from a warm-path probe when snap has them.
func (g Guest) EnsureScionFrom(ctx context.Context, spec ScionSpec, snap *Snapshot) error {
	// Validate before touching the guest, so a wrong config fails without
	// depending on the guest being up.
	if err := spec.Validate(); err != nil {
		return err
	}
	arch, ok := snap.GOARCH()
	if !ok {
		var err error
		if arch, err = g.GOARCH(ctx); err != nil {
			return fmt.Errorf("detect guest architecture: %w", err)
		}
	}
	bin, err, ok := spec.Prebuilt.Result(ctx, spec, arch, g.Machine)
	if !ok {
		bin, err = scionbin.Resolve(ctx, g.Host, spec, arch, g.Machine)
	}
	if err != nil {
		return err
	}
	guestDigest := func() string { return g.guestFileDigest(ctx, scionDestPath) }
	if d, ok := snap.Digest(scionDestPath); ok {
		guestDigest = func() string { return d }
	}
	if _, err := g.installRootBinaryIfChanged(ctx, bin, scionDestPath, guestDigest); err != nil {
		return err
	}
	return g.EnsureScionWebAssets(ctx, spec)
}

// ScionProbePath is the installed scion binary, for ProbeSpec.HashPaths.
const ScionProbePath = scionDestPath

// InstallRootBinary streams a host-local executable into the guest at destPath
// (mode +x), owned by root, via the RootPrefix transport.
//
// The install is atomic: a temp file is byte-count checked, then mv'd over
// destPath, so a mid-stream failure cannot leave a truncated executable (an
// early EOF makes `cat` exit 0, so the count check is what makes this true).
// destPath is shell-quoted because it is interpolated into the guest script.
//
// The file is opened ONCE and the same descriptor is hashed, then streamed:
// reopening by path would leave a window to point the path at another file.
func (g Guest) InstallRootBinary(ctx context.Context, localPath, destPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	defer func() { _ = f.Close() }()
	h, err := hashHandle(f)
	if err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	return g.installFromHandle(ctx, h, destPath)
}

// hashedHandle is an open host file with the digest and byte count computed
// from this same descriptor, rewound to the start.
type hashedHandle struct {
	f      *os.File
	size   int64
	digest string // hex sha256
}

func hashHandle(f *os.File) (hashedHandle, error) {
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return hashedHandle{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return hashedHandle{}, err
	}
	return hashedHandle{f: f, size: n, digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// installFromHandle re-hashes the bytes as they stream: a held descriptor
// cannot exclude a same-size in-place rewrite of the inode. The install
// cannot be undone by then, so the apply fails loudly instead, and the next
// apply re-streams because the guest digest no longer matches.
func (g Guest) installFromHandle(ctx context.Context, h hashedHandle, destPath string) error {
	sent := sha256.New()
	if err := g.pipeInto(ctx, g.RootPrefix, io.TeeReader(h.f, sent), installRootBinaryScript(destPath, h.size)); err != nil {
		return fmt.Errorf("install %s into guest: %w", destPath, err)
	}
	if got := hex.EncodeToString(sent.Sum(nil)); got != h.digest {
		return fmt.Errorf("install %s into guest: the host file changed while it was being streamed (verified sha256 %s, sent %s); "+
			"the guest holds unverified bytes — re-run apply", destPath, h.digest, got)
	}
	return nil
}

// installRootBinaryScript uses `wc -c <` rather than `stat` because the two
// stats disagree across GNU and BSD and the injection test runs this script
// on the host.
func installRootBinaryScript(destPath string, size int64) string {
	tmp := shellSingleQuote(destPath + ".tmp")
	return fmt.Sprintf("cat > %s && [ \"$(wc -c < %s)\" -eq %d ] && chmod +x %s && mv %s %s", tmp, tmp, size, tmp, tmp, shellSingleQuote(destPath))
}

// guestFileDigest returns "" when the digest cannot be read.
//
// sha256sum is called by absolute path: a bare name resolves on the guest
// user's PATH, which has run-user-writable directories ahead of /usr/bin, and
// a shim there could report the expected digest for a replaced binary.
func (g Guest) guestFileDigest(ctx context.Context, path string) string {
	res, err := g.UserRun(ctx, "/usr/bin/sha256sum", path)
	if err != nil {
		return ""
	}
	if f := strings.Fields(res.Stdout); len(f) > 0 {
		return f[0]
	}
	return ""
}

// InstallRootBinaryIfChanged installs localPath at destPath unless the guest
// already holds exactly those bytes, and reports whether it installed.
//
// It hashes the GUEST BINARY ITSELF, not a marker of a past install, so
// deletion, truncation and out-of-band replacement are all caught. Fails
// open: an unreadable guest digest means install. The host file is opened
// once, so the bytes streamed are the bytes whose digest decided the install.
func (g Guest) InstallRootBinaryIfChanged(ctx context.Context, localPath, destPath string) (bool, error) {
	return g.installRootBinaryIfChanged(ctx, localPath, destPath, func() string { return g.guestFileDigest(ctx, destPath) })
}

// installRootBinaryIfChanged takes the guest digest from the caller: read in
// place, or by the warm-path probe moments earlier.
func (g Guest) installRootBinaryIfChanged(ctx context.Context, localPath, destPath string, guestDigest func() string) (bool, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return false, fmt.Errorf("hashing %s: %w", localPath, err)
	}
	defer func() { _ = f.Close() }()
	h, err := hashHandle(f)
	if err != nil {
		return false, fmt.Errorf("hashing %s: %w", localPath, err)
	}
	if guestDigest() == h.digest {
		return false, nil
	}
	if err := g.installFromHandle(ctx, h, destPath); err != nil {
		return false, err
	}
	return true, nil
}

// shellSingleQuote wraps s in single quotes safe for POSIX shells.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
