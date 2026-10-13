// Package lima implements the Backend contract via a Lima VM (own kernel) +
// rootless podman + iptables egress.
//
// The prefix/guest-delegating half of the contract (the jail
// transport + image ops, the guest scion/egress delegations, run-user caching)
// lives once in internal/backend/common.Base, which this embeds; only the
// Lima-specific verbs (version preflight, VM create/start/stop, realized-config
// verification) and the two prefix/guest hooks are here.
package lima

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/state"
)

// hostAlias is the DNS name Lima resolves to the host from inside the VM.
const hostAlias = "host.lima.internal"

// limaVersionRe matches "limactl version 2.1.3" lines from `limactl --version`.
var limaVersionRe = regexp.MustCompile(`limactl version (\d+)\.(\d+)\.(\d+)`)

type Lima struct {
	common.Base
}

func New(r proc.Runner, vm string, opts common.Options) *Lima {
	l := &Lima{}
	l.Base = common.NewBase(common.Config{
		Runner:    r,
		Machine:   vm,
		HostAlias: hostAlias,
		Options:   opts,
		Hooks: common.Hooks{
			// Lima's jail prefix is static — it does not depend on the run user.
			JailPrefix: func(machine, _ string) []string { return JailPrefix(machine) },
			Guest: func(r proc.Runner, machine string) guest.Guest {
				return guest.Guest{
					Host:       r,
					UserPrefix: JailPrefix(machine),
					RootPrefix: append(JailPrefix(machine), "sudo"), // lima's default user has passwordless sudo
					Machine:    machine,
				}
			},
			ResolveHostAlias: l.resolveHostAlias,
			DNSForward:       l.resolverForward,
			DNSProbe: &guest.DNSProbe{
				NATChain:   limaDNSChain,
				ResolvConf: limaResolvConf,
				Forwards:   limaDNSForwards,
			},
		},
	})
	return l
}

// JailPrefix is the argv prefix that executes inside this backend's VM.
// Exported for registry.JailRunner (broker-side re-derivation).
func JailPrefix(vm string) []string { return []string{"limactl", "shell", vm} }

// Profile is lima's declared guarantees — the one value the runtime Profile()
// method, the registry's guarantee matrix and `lever backends` all report, so
// they cannot disagree.
var Profile = backend.Profile{
	Name:             "lima",
	SeparateKernel:   true, // own Lima VM kernel, not shared with the host or other jails
	FSBoundedBy:      "VM: no host files + project tree mounted at /lever",
	EgressEnforcedAt: "jail netns iptables/ip6tables",
	VersionFragile:   true, // depends on Lima's portForwards/mount behaviours
}

func (l *Lima) Profile() backend.Profile { return Profile }

func (l *Lima) EnsureUp(ctx context.Context, cfg backend.Config) error {
	if cfg.ProjectTree == "" {
		return fmt.Errorf("EnsureUp: ProjectTree is required")
	}
	// Preflight: require Lima >= 2.0.0. Lima 2.0 changed portForwards ignore
	// semantics — explicit guestIPMustBeZero is required from 2.0 on (see
	// template.go) — so an older limactl would silently forward guest ports to
	// the host loopback despite the rendered ignore rules.
	ok, got, err := common.VersionAtLeast(ctx, l.Runner(), []string{"limactl", "--version"}, limaVersionRe, 2, 0, 0)
	if err != nil {
		return fmt.Errorf("EnsureUp: limactl version check: %w", err)
	}
	if !ok {
		return fmt.Errorf("lever requires Lima >= 2.0.0 for portForwards ignore semantics; found %s", got)
	}
	if cfg.NestedVirt {
		if err := checkHostNested(hostArch, readHostFile); err != nil {
			return err
		}
	}
	if err := l.ensureVM(ctx, cfg); err != nil {
		return err
	}
	// Before Provision: on a fresh VM no egress chain exists yet, so the
	// HWE kernel install reaches the mirrors (a re-apply under the closed
	// posture may not, which the kernel step reports as a warning).
	rep := progress.From(ctx)
	s := rep.Start("guest hardening")
	err = l.hardenGuest(ctx, cfg)
	s.Done(err)
	if err != nil {
		return err
	}
	// Before Provision too: the small profile stops the apt timers that
	// would otherwise compete with provisioning for the dpkg lock.
	if err := l.ensureGuestProfile(ctx, cfg); err != nil {
		return err
	}
	if err := l.Provision(ctx, cfg); err != nil {
		return err
	}
	if cfg.NestedVirtUnknown {
		return nil
	}
	s = rep.Start("nested virt")
	if l.WarmSnapshot().NestedVirtConverged(cfg.NestedVirt) {
		s.Note("unchanged")
		s.Done(nil)
		return nil
	}
	err = l.Guest().EnsureNestedVirt(ctx, cfg.NestedVirt)
	s.Done(err)
	return err
}

// ensureVM creates the jail VM from the rendered template (template.go) if it
// doesn't yet exist, then starts it unless already running. The project tree
// mount is set only at `limactl create` time; changing it on an existing VM
// requires Teardown+EnsureUp — the same documented limitation as orbstack.
func (l *Lima) ensureVM(ctx context.Context, cfg backend.Config) error {
	rep := progress.From(ctx)
	status, err := l.vmStatus(ctx)
	if err != nil {
		return err
	}
	created := status == ""
	if created {
		s := rep.Start("create VM")
		err := l.createVM(ctx, cfg)
		s.Done(err)
		if err != nil {
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
	inst, err := l.verifyRealizedConfig(ctx, cfg.ProjectTree)
	if err != nil {
		return err
	}
	if !created && cfg.Warn != nil {
		for _, d := range inst.sizingDrift(cfg.CPUs, cfg.Memory) {
			cfg.Warn("jail: warning: %s; Lima applies it only when it creates the VM (to recreate: `lever conversation backup`, `lever destroy`, `lever up`, then `lever conversation restore`)", d)
		}
	}
	s := rep.Start("start VM")
	if status == "Running" {
		// Idempotent: already up (and just verified un-drifted).
		s.Skip("already running")
		return nil
	}
	// Lima's own log says what the boot waits for ("Waiting for the
	// essential requirement 1 of 4: ssh"); show it on the step, and its
	// tail if the start fails.
	haLog := filepath.Join(l.instanceDir(inst), "ha.stderr.log")
	s.TailFrom(func() (string, []string) { return haLog, lastLines(haLog, 15) })
	stopWatch := watchLimaStage(ctx, haLog, s.Detail)
	_, err = l.Runner().Run(ctx, nil, "limactl", "start", "--tty=false", l.Machine())
	stopWatch()
	if err != nil {
		err = fmt.Errorf("limactl start: %w", err)
	}
	s.Done(err)
	return err
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
	// Dir is the instance's directory (its logs live there); progress
	// output only, never the containment check.
	Dir    string `json:"dir"`
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
		// CPUs and Memory are read for the sizing-drift warning only, never
		// for the containment check.
		CPUs   *int    `json:"cpus"`
		Memory *string `json:"memory"`
	} `json:"config"`
}

// sizingDrift describes each configured size (cpus, memory; 0 and "" mean
// unset) that differs from the realized VM's. A value lever cannot read or
// parse is not reported: the warning is advice, and a wrong one is worse
// than none.
func (inst realizedInstance) sizingDrift(cpus int, memory string) []string {
	var out []string
	if c := inst.Config.CPUs; cpus != 0 && c != nil && *c != cpus {
		out = append(out, fmt.Sprintf("cpus is %d but the existing VM has %d", cpus, *c))
	}
	if m := inst.Config.Memory; memory != "" && m != nil {
		want, ok1 := memBytes(memory)
		got, ok2 := memBytes(*m)
		if ok1 && ok2 && want != got {
			out = append(out, fmt.Sprintf("memory is %s but the existing VM has %s", memory, *m))
		}
	}
	return out
}

var memRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(B|KiB|MiB|GiB|TiB|KB|MB|GB|TB)$`)

// memBytes parses a size as lever's config and Lima write it ("24GiB",
// "4096MiB", Lima's fractional "3.906GiB"), to whole bytes.
func memBytes(s string) (int64, bool) {
	m := memRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	unit := map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}[m[2]]
	return int64(n * unit), true
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
	if c.Mounts[0].Location != projectTree || c.Mounts[0].MountPoint != common.MountDest || !c.Mounts[0].Writable {
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
// template's intent for projectTree. It returns the config it read.
func (l *Lima) verifyRealizedConfig(ctx context.Context, projectTree string) (realizedInstance, error) {
	var inst realizedInstance
	res, err := l.Runner().Run(ctx, nil, "limactl", "list", "--json", l.Machine())
	if err != nil {
		return inst, fmt.Errorf("read back realized config for %q: %w", l.Machine(), err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &inst); err != nil {
		return inst, fmt.Errorf("parse realized config for %q: %w", l.Machine(), err)
	}
	if !inst.matchesContainment(projectTree) {
		return inst, fmt.Errorf("lima VM %q exists with a mismatched containment config (mounts/port-forwards/containerd drifted from the lever template); run 'lever down' then 'lever up' to recreate", l.Machine())
	}
	return inst, nil
}

// vmStatus returns this VM's status field from `limactl list`, or "" if the
// VM is not listed at all.
func (l *Lima) vmStatus(ctx context.Context) (string, error) {
	res, err := l.Runner().Run(ctx, nil, "limactl", "list", "--format", "{{.Name}} {{.Status}}")
	if err != nil {
		return "", fmt.Errorf("limactl list: %w", err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == l.Machine() {
			return f[1], nil
		}
	}
	return "", nil
}

// createVM renders the containment template to a temp file and creates the VM
// from it. `limactl create` reads the config file path as a positional arg;
// the temp file is removed afterward since lima reads it once at create time.
func (l *Lima) createVM(ctx context.Context, cfg backend.Config) error {
	out, err := RenderTemplate(cfg.ProjectTree, TemplateOpts{Disk: cfg.Disk, CPUs: cfg.CPUs, Memory: cfg.Memory, NestedVirt: cfg.NestedVirt})
	if err != nil {
		return fmt.Errorf("render lima template: %w", err)
	}
	f, err := os.CreateTemp(os.TempDir(), "lever-lima-*.yaml")
	if err != nil {
		return fmt.Errorf("create lima config tempfile: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(out); err != nil {
		f.Close()
		return fmt.Errorf("write lima config tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close lima config tempfile: %w", err)
	}
	if _, err := l.Runner().Run(ctx, nil, "limactl", "create", "--name="+l.Machine(), "--tty=false", path); err != nil {
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
		return fmt.Errorf("lima VM %q does not exist", l.Machine())
	}
	if status != "Running" {
		return fmt.Errorf("lima VM %q is not running (status %q)", l.Machine(), status)
	}
	return l.ReadRunUser(ctx)
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
	if _, err := l.Runner().Run(ctx, nil, "limactl", "delete", "--force", l.Machine()); err != nil {
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
	if _, err := l.Runner().Run(ctx, nil, "limactl", "stop", l.Machine()); err != nil {
		return fmt.Errorf("limactl stop: %w", err)
	}
	return nil
}

// resolveHostAlias returns the IPv4 and IPv6 addresses host.lima.internal
// resolves to FROM INSIDE the VM (both forward to the host's 127.0.0.1). It is
// the Base ResolveHostAlias hook (and is exercised directly by lima_test.go).
func (l *Lima) resolveHostAlias(ctx context.Context) (v4, v6 string, err error) {
	res, err := l.Runner().Run(ctx, nil, "limactl", "shell", l.Machine(), "getent", "ahosts", hostAlias)
	if err != nil {
		return "", "", fmt.Errorf("getent %s: %w", hostAlias, err)
	}
	v4, v6 = guest.ParseAhosts(res.Stdout)
	if v4 == "" && v6 == "" {
		return "", "", fmt.Errorf("%s resolved to no addresses", hostAlias)
	}
	return v4, v6, nil
}

// limaDNSChain is the nat-table chain Lima's guest boot script
// (cidata boot.Linux/09-host-dns-setup.sh) fills with the DNAT that routes the
// guest resolver address (192.168.5.3:53) to the host agent's DNS server on
// <host alias>:<per-boot port>. It is inserted first in nat OUTPUT, so the
// filter OUTPUT chain (LEVER_EGRESS) sees the rewritten destination.
const limaDNSChain = "LIMADNS"

// resolverForward is the Base DNSForward hook (lever#34): it reads the live
// LIMADNS chain and returns its DNAT targets on the alias, so ApplyEgress can
// ACCEPT them ahead of the alias DROP in the open posture. The ports change on
// every VM boot and lever re-applies egress on every `up`, so reading them at
// apply time is exact. A missing chain (hostResolver disabled, or a Lima that
// routes DNS differently) yields no targets rather than an error: the alias
// DROP then stays as it was, and doctor's guest-DNS row reports the outcome.
//
// With vmType vz (lever's default on macOS) LIMADNS is empty: the guest's
// systemd-resolved talks to <alias>:53 directly. So when the chain yields no
// targets, the resolver's upstream file is read, and an alias nameserver
// gives the port-53 targets (egress.ParseResolverUpstreams).
func (l *Lima) resolverForward(ctx context.Context, aliasV4 string) ([]egress.DNSForward, error) {
	nat, natErr := l.Guest().RootRun(ctx, "iptables", "-t", "nat", "-S", limaDNSChain)
	if natErr == nil {
		if targets := egress.ParseDNATTargets(nat.Stdout, aliasV4); len(targets) > 0 {
			return targets, nil
		}
	}
	res, err := l.Guest().RootRun(ctx, "cat", limaResolvConf)
	return limaDNSForwards(nat.Stdout, natErr == nil, res.Stdout, err == nil, aliasV4), nil
}

// limaResolvConf is systemd-resolved's upstream file, read when LIMADNS
// yields no targets.
const limaResolvConf = "/run/systemd/resolve/resolv.conf"

// limaDNSForwards is resolverForward's decision on what it read, shared with
// the warm-path probe (guest.DNSProbe): the LIMADNS targets when the chain
// was read and has any, else the resolver upstream's, else none.
func limaDNSForwards(nat string, natOK bool, resolv string, resolvOK bool, aliasV4 string) []egress.DNSForward {
	if natOK {
		if targets := egress.ParseDNATTargets(nat, aliasV4); len(targets) > 0 {
			return targets
		}
	}
	if !resolvOK {
		return nil
	}
	return egress.ParseResolverUpstreams(resolv, aliasV4)
}

var _ backend.Backend = (*Lima)(nil)

// RebootAdvice is how an operator reboots the jail VM without losing the
// manager's conversation.
const RebootAdvice = "`lever stop`, then `lever up`, reboots the VM and keeps the manager conversation"

// RecreateAdvice is how an operator replaces the jail VM (a new image, a
// create-time setting) and keeps the manager conversation.
const RecreateAdvice = "`lever conversation backup`, then `lever destroy` (deletes the VM and the conversation) and `lever up`, then `lever conversation restore`"

// ensureGuestProfile converges the guest on cfg.GuestProfile (lever#193):
// the small profile's trimmed services and zram swap, or standard, which
// undoes them. "" (no config) skips the step. What systemd refuses is a
// warning; a failed write of lever's own files fails the bring-up.
func (l *Lima) ensureGuestProfile(ctx context.Context, cfg backend.Config) error {
	if cfg.GuestProfile == "" {
		return nil
	}
	warn := cfg.Warn
	if warn == nil {
		warn = func(string, ...any) {}
	}
	s := progress.From(ctx).Start("guest profile " + cfg.GuestProfile)
	rep, err := l.Guest().EnsureGuestProfile(ctx, cfg.GuestProfile)
	s.Done(err)
	if err != nil {
		return err
	}
	for _, w := range rep.Warn {
		warn("jail: warning: %s", w)
	}
	for _, r := range rep.Reboot {
		warn("jail: reboot pending: %s; %s", r, RebootAdvice)
	}
	return nil
}

// hardenGuest converges the guest's crash recovery (always) and its kernel
// flavour (cfg.GuestKernel) on every bring-up, so an existing VM gets them
// on its next apply or up (lever#179). `lever up` runs it too, whatever
// the manager's state, because up builds its dependencies through EnsureUp
// before it decides to resume or attach. A failed crash-recovery write
// fails the bring-up; what only a reboot applies, and a kernel install
// that could not run, are warnings: lever never reboots the guest itself.
func (l *Lima) hardenGuest(ctx context.Context, cfg backend.Config) error {
	warn := cfg.Warn
	if warn == nil {
		warn = func(string, ...any) {}
	}
	crash, err := l.Guest().EnsureCrashRecovery(ctx)
	if err != nil {
		return err
	}
	var opts guest.KernelOpts
	stamp, stampErr := loadHWEStamp(cfg.HWEFailStamp)
	if stampErr != nil {
		warn("jail: warning: %v", stampErr)
	}
	if cfg.GuestKernel == guest.GuestKernelHWE && cfg.ClosedInternet {
		opts.SkipIfMachineID = stamp.MachineID
	}
	kernel, err := l.Guest().EnsureGuestKernel(ctx, cfg.GuestKernel, opts)
	if err != nil {
		// The kernel step only installs a package and reads two paths; a
		// failure to run it must not keep the jail down.
		warn("jail: warning: %v", err)
	}
	if cfg.GuestKernel == guest.GuestKernelHWE {
		l.keepHWEStamp(cfg.HWEFailStamp, stamp, kernel, warn)
	}
	for _, w := range append(crash.Warn, kernel.Warn...) {
		warn("jail: warning: %s", w)
	}
	for _, r := range append(crash.Reboot, kernel.Reboot...) {
		warn("jail: reboot pending: %s; %s", r, RebootAdvice)
	}
	return nil
}

// hweStamp is the host-side record of a guest_kernel hwe install that
// failed (backend.Config.HWEFailStamp): the VM it failed in, by
// /etc/machine-id (stable for the VM's life, new when it is recreated),
// and when.
type hweStamp struct {
	MachineID string    `json:"machine_id"`
	FailedAt  time.Time `json:"failed_at"`
}

// loadHWEStamp reads the stamp; a missing path or file is the zero stamp.
func loadHWEStamp(path string) (hweStamp, error) {
	if path == "" {
		return hweStamp{}, nil
	}
	s, err := state.LoadJSON[hweStamp](path, "HWE install stamp")
	if err != nil {
		return hweStamp{}, fmt.Errorf("%v (delete %s to retry the HWE kernel install)", err, path)
	}
	return s, nil
}

// keepHWEStamp converges the stamp on what the kernel step reported: a
// failed install stamps the VM, an installed package removes the stamp,
// and a skip on the stamp says what to do. Writes are best-effort
// (warned): the stamp only saves time.
func (l *Lima) keepHWEStamp(path string, was hweStamp, k guest.KernelReport, warn func(string, ...any)) {
	if path == "" {
		return
	}
	switch k.HWE {
	case guest.HWEFailed:
		if k.MachineID == "" {
			return
		}
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = state.SaveJSON(path, "HWE install stamp", hweStamp{MachineID: k.MachineID, FailedAt: time.Now().UTC()})
		}
		if err != nil {
			warn("jail: warning: %v", err)
		}
	case guest.HWEInstalled:
		if was.MachineID != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				warn("jail: warning: remove %s: %v", path, err)
			}
		}
	case guest.HWESkippedStamp:
		pkg := k.HWEPackage
		if pkg == "" {
			pkg = "the HWE kernel package"
		}
		warn("jail: warning: guest_kernel hwe: %s is not installed; the install failed in this VM on %s, and lever does not run apt again while the egress posture is closed (apt would wait up to %d s each time). Run lever apply once with the open posture, install it by hand in the guest, or delete %s to retry",
			pkg, was.FailedAt.Format(time.RFC3339), guest.AptLockWait, path)
	}
}
