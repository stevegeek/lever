package lima

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/jail"
)

const (
	// mountDest is the path inside the VM where the project tree is bind-
	// mounted, set at `limactl create` time by the rendered template
	// (template.go).
	mountDest = "/lever"
	// defaultRunUID is the fallback UID used for the rootless Docker socket
	// path before EnsureUp has resolved the real UID. Lima's guest user
	// mirrors the host user, and macOS hosts are UID 501, so this default is
	// sensible even before EnsureUp, per the interface's "valid after
	// EnsureUp" contract.
	defaultRunUID = "501"
	// hostAlias is the DNS name Lima resolves to the host from inside the VM.
	hostAlias = "host.lima.internal"
)

// limaVersionRe matches "limactl version 2.1.3" lines from `limactl --version`.
var limaVersionRe = regexp.MustCompile(`limactl version (\d+)\.(\d+)\.(\d+)`)

type Lima struct {
	r       exec.Runner
	vm      string
	aliasV4 string
	aliasV6 string
	runUser string // resolved via `limactl shell <vm> whoami`
	runUID  string // resolved via `limactl shell <vm> id -u`
}

func New(r exec.Runner, vm string) *Lima { return &Lima{r: r, vm: vm} }

// JailPrefix is the argv prefix that executes inside this backend's VM.
// Exported for registry.JailRunner (broker-side re-derivation).
func JailPrefix(vm string) []string { return []string{"limactl", "shell", vm} }

// guest returns the shared guest provisioner scoped to this VM, used to
// install runtimes and scion via host-side argv prefixes. See
// internal/backend/guest for the provisioning scripts themselves.
func (l *Lima) guest() guest.Guest {
	return guest.Guest{
		Host:       l.r,
		UserPrefix: JailPrefix(l.vm),
		RootPrefix: append(JailPrefix(l.vm), "sudo"), // lima's default user has passwordless sudo
		Machine:    l.vm,
	}
}

// Profile returns lima's declared guarantees. The value lives once in
// backend.Candidates (the single source of the guarantee matrix); returning it
// here keeps the runtime profile and the documented one identical.
func (l *Lima) Profile() backend.Profile {
	p, _ := backend.ProfileFor("lima")
	return p
}

func (l *Lima) DockerHost() string {
	uid := l.runUID
	if uid == "" {
		uid = defaultRunUID
	}
	return fmt.Sprintf("unix:///run/user/%s/docker.sock", uid)
}

func (l *Lima) HostToolAlias() string { return hostAlias }

// HostAliasV4 returns the resolved IPv4 of host.lima.internal as seen from
// the jail, valid after EnsureUp/ApplyEgress. Empty if not yet resolved. Used
// to mint the broker cert with an IP SAN and to build IP-based broker URLs so
// agents reach the broker without DNS under closed-internet egress.
func (l *Lima) HostAliasV4() string { return l.aliasV4 }

func (l *Lima) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if cfg.ProjectTree == "" {
		return fmt.Errorf("EnsureUp: ProjectTree is required")
	}
	// Preflight: require Lima >= 2.0.0. Lima 2.0 changed portForwards ignore
	// semantics — explicit guestIPMustBeZero is required from 2.0 on (see
	// template.go) — so an older limactl would silently forward guest ports to
	// the host loopback despite the rendered ignore rules.
	ok, got, err := limaVersionAtLeast(ctx, l.r, 2, 0, 0)
	if err != nil {
		return fmt.Errorf("EnsureUp: limactl version check: %w", err)
	}
	if !ok {
		return fmt.Errorf("lever requires Lima >= 2.0.0 for portForwards ignore semantics; found %s", got)
	}
	if err := l.ensureVM(ctx, cfg.ProjectTree, cfg.Disk); err != nil {
		return err
	}
	if err := l.resolveRunUser(ctx); err != nil {
		return err
	}
	if err := l.guest().EnsureRuntimes(ctx, l.runUser); err != nil {
		return err
	}
	if cfg.ScionSource != "" || cfg.ScionVersion != "" {
		if err := l.guest().EnsureScion(ctx, cfg.ScionSource, cfg.ScionVersion); err != nil {
			return err
		}
	}
	return l.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
}

// limaVersionAtLeast runs `limactl --version`, parses the semver, and returns
// whether it is >= (major, minor, patch). got is the raw version string on
// success or the raw output on parse failure. This mirrors orbstack's
// orbVersionAtLeast compare exactly (same three-way major/minor/patch
// switch); it is copied here rather than hoisted into guest because it is the
// only other call site — hoisting a generic compare for one duplicate would
// be churn without payoff.
func limaVersionAtLeast(ctx context.Context, r exec.Runner, major, minor, patch int) (ok bool, got string, err error) {
	res, err := r.Run(ctx, nil, "limactl", "--version")
	if err != nil {
		return false, "", fmt.Errorf("limactl --version: %w", err)
	}
	m := limaVersionRe.FindStringSubmatch(res.Stdout)
	if m == nil {
		return false, strings.TrimSpace(res.Stdout), fmt.Errorf("limactl --version: could not parse version from %q", strings.TrimSpace(res.Stdout))
	}
	// m[1],m[2],m[3] are guaranteed digits by the regex.
	vMaj, _ := strconv.Atoi(m[1])
	vMin, _ := strconv.Atoi(m[2])
	vPat, _ := strconv.Atoi(m[3])
	got = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])

	switch {
	case vMaj > major:
		return true, got, nil
	case vMaj < major:
		return false, got, nil
	// vMaj == major
	case vMin > minor:
		return true, got, nil
	case vMin < minor:
		return false, got, nil
	// vMin == minor
	default:
		return vPat >= patch, got, nil
	}
}

// ensureVM creates the jail VM from the rendered template (template.go) if it
// doesn't yet exist, then starts it unless already running. The project tree
// mount is set only at `limactl create` time; changing it on an existing VM
// requires Teardown+EnsureUp — the same documented limitation as orbstack.
func (l *Lima) ensureVM(ctx context.Context, projectTree, disk string) error {
	status, err := l.vmStatus(ctx)
	if err != nil {
		return err
	}
	if status == "" {
		if err := l.createVM(ctx, projectTree, disk); err != nil {
			return err
		}
		status = "Stopped" // freshly created, not yet started
	}
	// Verify the REALIZED containment config on every path that reaches a
	// live VM — a fresh create (belt-and-braces: a global lima config could
	// still widen it) AND, critically, an ADOPTED pre-existing VM (one from a
	// prior run, one booted before a template fix, or one an operator's
	// global ~/.lima/_config/override.yaml has widened). mounts/portForwards/
	// containerd are `limactl create`-time only, so without this check an
	// adopted VM is used wholesale with no drift check even though the
	// template IS the containment surface (template.go).
	if err := l.verifyRealizedConfig(ctx, projectTree); err != nil {
		return err
	}
	if status == "Running" {
		// Idempotent: already up (and just verified un-drifted).
		return nil
	}
	if _, err := l.r.Run(ctx, nil, "limactl", "start", "--tty=false", l.vm); err != nil {
		return fmt.Errorf("limactl start: %w", err)
	}
	return nil
}

// realizedInstance is the subset of `limactl list --json <vm>` needed to
// verify the containment surface. Deliberately NOT read from the raw
// ~/.lima/<vm>/lima.yaml lima persists at `limactl create` time: that file
// holds the pre-merge template bytes only. `limactl list --json` goes through
// lima's store.Inspect, which re-loads and re-merges the instance config with
// any ~/.lima/_config/{default,override}.yaml on EVERY call — that merged
// result is what lima actually applies the next time the VM starts, so it is
// the only read-back that can catch a global override widening the surface
// (see security-model-jail.md §2.4's lima operational notes).
type realizedInstance struct {
	Config struct {
		Mounts []struct {
			Location   string `json:"location"`
			MountPoint string `json:"mountPoint"`
			Writable   bool   `json:"writable"`
		} `json:"mounts"`
		PortForwards []struct {
			GuestIP           string `json:"guestIP"`
			GuestIPMustBeZero bool   `json:"guestIPMustBeZero"`
			GuestPortRange    []int  `json:"guestPortRange"`
			Proto             string `json:"proto"`
			Ignore            bool   `json:"ignore"`
		} `json:"portForwards"`
		Containerd struct {
			System bool `json:"system"`
			User   bool `json:"user"`
		} `json:"containerd"`
	} `json:"config"`
}

// matchesContainment reports whether the realized config is exactly the
// containment surface template.go renders for projectTree: one writable
// mount at mountDest, exactly the two full-range proto:any ignore rules (see
// FIX 1), and containerd fully disabled.
func (inst realizedInstance) matchesContainment(projectTree string) bool {
	c := inst.Config
	if len(c.Mounts) != 1 {
		return false
	}
	if c.Mounts[0].Location != projectTree || c.Mounts[0].MountPoint != mountDest || !c.Mounts[0].Writable {
		return false
	}
	if c.Containerd.System || c.Containerd.User {
		return false
	}
	if len(c.PortForwards) != 2 {
		return false
	}
	var sawZero, sawLoopback bool
	for _, pf := range c.PortForwards {
		if !pf.Ignore || pf.Proto != "any" ||
			len(pf.GuestPortRange) != 2 || pf.GuestPortRange[0] != 1 || pf.GuestPortRange[1] != 65535 {
			return false
		}
		switch pf.GuestIP {
		case "0.0.0.0":
			if !pf.GuestIPMustBeZero {
				return false
			}
			sawZero = true
		case "127.0.0.1":
			sawLoopback = true
		default:
			return false
		}
	}
	return sawZero && sawLoopback
}

// verifyRealizedConfig reads back the VM's realized config (see
// realizedInstance) and fails closed unless it matches the containment
// template's intent for projectTree.
func (l *Lima) verifyRealizedConfig(ctx context.Context, projectTree string) error {
	res, err := l.r.Run(ctx, nil, "limactl", "list", "--json", l.vm)
	if err != nil {
		return fmt.Errorf("read back realized config for %q: %w", l.vm, err)
	}
	var inst realizedInstance
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &inst); err != nil {
		return fmt.Errorf("parse realized config for %q: %w", l.vm, err)
	}
	if !inst.matchesContainment(projectTree) {
		return fmt.Errorf("lima VM %q exists with a mismatched containment config (mounts/port-forwards/containerd drifted from the lever template); run 'lever down' then 'lever up' to recreate", l.vm)
	}
	return nil
}

// vmStatus returns this VM's status field from `limactl list`, or "" if the
// VM is not listed at all.
func (l *Lima) vmStatus(ctx context.Context) (string, error) {
	res, err := l.r.Run(ctx, nil, "limactl", "list", "--format", "{{.Name}} {{.Status}}")
	if err != nil {
		return "", fmt.Errorf("limactl list: %w", err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == l.vm {
			return f[1], nil
		}
	}
	return "", nil
}

// createVM renders the containment template to a temp file and creates the VM
// from it. `limactl create` reads the config file path as a positional arg;
// the temp file is removed afterward since lima reads it once at create time.
func (l *Lima) createVM(ctx context.Context, projectTree, disk string) error {
	cfg, err := RenderTemplate(projectTree, disk)
	if err != nil {
		return fmt.Errorf("render lima template: %w", err)
	}
	f, err := os.CreateTemp(os.TempDir(), "lever-lima-*.yaml")
	if err != nil {
		return fmt.Errorf("create lima config tempfile: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(cfg); err != nil {
		f.Close()
		return fmt.Errorf("write lima config tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close lima config tempfile: %w", err)
	}
	if _, err := l.r.Run(ctx, nil, "limactl", "create", "--name="+l.vm, "--tty=false", path); err != nil {
		return fmt.Errorf("limactl create: %w", err)
	}
	return nil
}

// ResolveRunUser resolves the in-VM run user/uid WITHOUT provisioning: it
// probes the VM's status via the same read-only `limactl list` check ensureVM
// uses, and errors if the VM is absent or not running, rather than creating,
// starting, or reconfiguring it. For passive verbs (attach) that need the
// jail transport but must never bring the VM up.
func (l *Lima) ResolveRunUser(ctx context.Context) error {
	status, err := l.vmStatus(ctx)
	if err != nil {
		return err
	}
	if status == "" {
		return fmt.Errorf("lima VM %q does not exist", l.vm)
	}
	if status != "Running" {
		return fmt.Errorf("lima VM %q is not running (status %q)", l.vm, status)
	}
	return l.resolveRunUser(ctx)
}

// resolveRunUser caches the in-VM run user and UID so the rootless Docker
// socket path and the subid/linger script work for any Lima guest user, not a
// hardcoded one. Called after ensureVM (the VM must be up) and before
// EnsureRuntimes.
func (l *Lima) resolveRunUser(ctx context.Context) error {
	res, err := l.r.Run(ctx, nil, "limactl", "shell", l.vm, "whoami")
	if err != nil {
		return fmt.Errorf("resolve run user: %w", err)
	}
	l.runUser = strings.TrimSpace(res.Stdout)
	res, err = l.r.Run(ctx, nil, "limactl", "shell", l.vm, "id", "-u")
	if err != nil {
		return fmt.Errorf("resolve run uid: %w", err)
	}
	l.runUID = strings.TrimSpace(res.Stdout)
	return nil
}

// Teardown deletes the jail VM. Idempotent: a no-op if the VM is already
// absent.
func (l *Lima) Teardown(ctx context.Context) error {
	status, err := l.vmStatus(ctx)
	if err != nil {
		return err
	}
	if status == "" {
		return nil // already gone
	}
	if _, err := l.r.Run(ctx, nil, "limactl", "delete", "--force", l.vm); err != nil {
		return fmt.Errorf("limactl delete: %w", err)
	}
	return nil
}

// Stop powers the VM off but keeps its disk intact — a strictly less
// destructive operation than Teardown (which deletes the VM). Idempotent: a
// no-op if the VM is already absent; limactl tolerates stopping an
// already-stopped VM, so no separate guard is needed for that case.
func (l *Lima) Stop(ctx context.Context) error {
	status, err := l.vmStatus(ctx)
	if err != nil {
		return err
	}
	if status == "" {
		return nil // already gone; nothing to stop
	}
	if _, err := l.r.Run(ctx, nil, "limactl", "stop", l.vm); err != nil {
		return fmt.Errorf("limactl stop: %w", err)
	}
	return nil
}

// resolveHostAlias returns the IPv4 and IPv6 addresses host.lima.internal
// resolves to FROM INSIDE the VM (both forward to the host's 127.0.0.1).
func (l *Lima) resolveHostAlias(ctx context.Context) (v4, v6 string, err error) {
	res, err := l.r.Run(ctx, nil, "limactl", "shell", l.vm, "getent", "ahosts", hostAlias)
	if err != nil {
		return "", "", fmt.Errorf("getent %s: %w", hostAlias, err)
	}
	v4, v6 = guest.ParseAhosts(res.Stdout)
	if v4 == "" && v6 == "" {
		return "", "", fmt.Errorf("%s resolved to no addresses", hostAlias)
	}
	return v4, v6, nil
}

func (l *Lima) ApplyEgress(ctx context.Context, allowedPorts []int, closedInternet bool) error {
	v4, v6, rebuilt, err := l.guest().ApplyEgress(ctx, l.resolveHostAlias, allowedPorts, closedInternet)
	if err != nil {
		return err
	}
	if rebuilt {
		l.aliasV4, l.aliasV6 = v4, v6
	} else {
		// I2 skip path: v6 is not authoritative here (existingClosedAlias only
		// parses v4 from the live chain) — do not clobber a prior aliasV6.
		l.aliasV4 = v4
	}
	return nil
}

// MountDest returns the path inside the jail where the project tree is bind-mounted.
func (l *Lima) MountDest() string { return mountDest }

// MachineName returns the jail VM name this backend targets.
func (l *Lima) MachineName() string { return l.vm }

// RunUser returns the in-VM run user resolved by EnsureUp (valid after EnsureUp).
func (l *Lima) RunUser() string { return l.runUser }

// RunUID returns the in-VM run user's UID resolved by EnsureUp. Falls back to
// defaultRunUID if EnsureUp has not yet been called.
func (l *Lima) RunUID() string {
	if l.runUID == "" {
		return defaultRunUID
	}
	return l.runUID
}

// JailRunner returns the command transport into the jail (valid after EnsureUp,
// which resolves the run user's uid).
func (l *Lima) JailRunner() exec.Runner {
	return jail.New(l.r, JailPrefix(l.vm), l.RunUID())
}

// AttachArgv builds the host argv for an interactive in-jail command.
func (l *Lima) AttachArgv(inner []string) []string {
	return jail.AttachArgv(JailPrefix(l.vm), l.RunUID(), inner)
}

// LoadImage streams a host docker image into the jail's rootless podman.
func (l *Lima) LoadImage(ctx context.Context, imageRef string) error {
	return jail.LoadImage(ctx, JailPrefix(l.vm), l.RunUID(), imageRef)
}

// ImageLoaded reports whether the jail already holds imageRef at the host's
// image ID (so a re-import can be skipped). Fail-open — see the interface doc.
func (l *Lima) ImageLoaded(ctx context.Context, imageRef string) bool {
	return jail.ImageLoaded(ctx, JailPrefix(l.vm), l.RunUID(), imageRef)
}

// PruneJailImages reclaims dangling images from the jail's rootless podman.
func (l *Lima) PruneJailImages(ctx context.Context) error {
	return jail.PruneImages(ctx, JailPrefix(l.vm), l.RunUID())
}

// InstallGuestBinary streams a host-local executable into the VM at destPath as
// root, via the shared guest provisioner (RootPrefix = `limactl shell <vm> sudo`).
func (l *Lima) InstallGuestBinary(ctx context.Context, localPath, destPath string) error {
	return l.guest().InstallRootBinary(ctx, localPath, destPath)
}

// ReadScionProjectState reads scion's registration state from the VM for `lever
// doctor` (in-tree marker + ~/.scion/project-configs). Read-only via the
// machine-only guest prefix, so it needs no EnsureUp.
func (l *Lima) ReadScionProjectState(ctx context.Context) (backend.ScionProjectState, error) {
	return l.guest().ReadScionProjectState(ctx, mountDest)
}

// RemoveScionProjectConfigs removes stale scion project-config registrations
// for wp from the VM, via the machine-only guest prefix.
func (l *Lima) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	return l.guest().RemoveScionProjectConfigs(ctx, wp)
}

// ScionProjectRegistered reports whether workspacePath already has exactly one
// valid scion registration, via the machine-only guest prefix. Read-only, no
// EnsureUp.
func (l *Lima) ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error) {
	return l.guest().ScionProjectRegistered(ctx, workspacePath)
}

var _ backend.Backend = (*Lima)(nil)
