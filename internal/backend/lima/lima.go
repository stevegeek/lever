// Package lima implements the Backend contract via a Lima VM (own kernel) +
// rootless podman + iptables egress. The shared half lives in
// internal/backend/common.Base, which this embeds.
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

const hostAlias = "host.lima.internal"

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
func JailPrefix(vm string) []string { return []string{"limactl", "shell", vm} }

// Profile is lima's declared guarantees, the single value every report uses.
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
	// Lima < 2.0 lacks guestIPMustBeZero ignore semantics and would silently
	// forward guest ports to the host loopback despite the ignore rules.
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
	// HWE kernel install can reach the mirrors.
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

// ensureVM: the project tree mount is set only at `limactl create` time;
// changing it on an existing VM requires Teardown+EnsureUp.
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
	// live VM, created or adopted: a global ~/.lima/_config/override.yaml or
	// an older template can widen it, and mounts/portForwards/containerd are
	// fixed at `limactl create` time.
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
		if !created {
			l.saveGuestLogs(cfg, inst, true)
		}
		s.Skip("already running")
		return nil
	}
	if !created {
		l.saveGuestLogs(cfg, inst, false)
	}
	// Lima's own log says what the boot waits for ("Waiting for the
	// essential requirement 1 of 4: ssh").
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

// saveGuestLogs never fails the bring-up: the logs are evidence for the
// operator, not part of containment.
func (l *Lima) saveGuestLogs(cfg backend.Config, inst realizedInstance, running bool) {
	if cfg.GuestLogDir == "" {
		return
	}
	save := SaveGuestLogs
	if running {
		save = SaveLivePanic
	}
	path, err := save(l.instanceDir(inst), cfg.GuestLogDir, time.Now())
	if cfg.Warn == nil {
		return
	}
	if err != nil {
		cfg.Warn("jail: warning: saving the VM's console logs: %v", err)
	}
	if running && path != "" {
		cfg.Warn("jail: warning: the VM's console shows a guest kernel panic since the VM started; saved its console logs to %s (`lever doctor`, row \"last guest boot\")", path)
	}
}

// realizedInstance is the subset of `limactl list --json <vm>` needed to
// verify the containment surface. Deliberately NOT read from
// ~/.lima/<vm>/lima.yaml, which holds the pre-merge template only: `limactl
// list --json` re-merges ~/.lima/_config/{default,override}.yaml on every
// call, so it is the only read-back that catches a global override widening
// the surface.
type realizedInstance struct {
	// Dir is for progress output only, never the containment check.
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

// sizingDrift skips a value it cannot read or parse: the warning is advice,
// and a wrong one is worse than none.
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

// memBytes accepts Lima's fractional form ("3.906GiB") too.
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
// mount at MountDest, exactly the two full-range proto:any ignore rules, and
// containerd fully disabled.
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

// verifyRealizedConfig fails closed unless the realized config matches the
// containment template for projectTree.
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

// vmStatus returns "" if the VM is not listed at all.
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

// ResolveRunUser errors if the VM is absent or not running, rather than
// creating, starting or reconfiguring it.
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

// Teardown deletes the jail VM. Idempotent.
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

// Stop powers the VM off but keeps its disk. Idempotent: limactl tolerates
// stopping an already-stopped VM.
// MachineStopped reports whether the VM is stopped or absent, so nothing
// in it can write the tree. Any other status, unknown ones included, is not
// stopped.
func (l *Lima) MachineStopped(ctx context.Context) (bool, error) {
	status, err := l.vmStatus(ctx)
	if err != nil {
		return false, err
	}
	return status == "" || status == "Stopped", nil
}

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

// resolveHostAlias resolves the alias FROM INSIDE the VM; both addresses
// forward to the host's 127.0.0.1.
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

// limaDNSChain is the nat-table chain Lima's guest boot script fills with the
// DNAT that routes the guest resolver (192.168.5.3:53) to the host agent's DNS
// server on <host alias>:<per-boot port>. It is first in nat OUTPUT, so
// LEVER_EGRESS sees the rewritten destination.
const limaDNSChain = "LIMADNS"

// resolverForward returns the DNS targets on the alias that ApplyEgress
// ACCEPTs ahead of the alias DROP in the open posture. The LIMADNS ports
// change on every VM boot, and egress is re-applied on every `up`, so reading
// them at apply time is exact. A missing chain yields no targets rather than
// an error: the alias DROP then stays as it was.
//
// With vmType vz LIMADNS is empty and systemd-resolved talks to <alias>:53
// directly, so the resolver's upstream file is read instead.
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

const limaResolvConf = "/run/systemd/resolve/resolv.conf"

// limaDNSForwards is shared with the warm-path probe (guest.DNSProbe) so both
// decide identically.
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

// ensureGuestProfile: what systemd refuses is a warning; a failed write of
// lever's own files fails the bring-up.
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

// hardenGuest converges the guest's crash recovery and kernel flavour on every
// bring-up. A failed crash-recovery write fails the bring-up; what only a
// reboot applies, and a kernel install that could not run, are warnings:
// lever never reboots the guest itself.
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
		// A failure to run the kernel step must not keep the jail down.
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

// hweStamp records a failed guest_kernel hwe install. MachineID is the
// guest's /etc/machine-id: stable for the VM's life, new when it is
// recreated.
type hweStamp struct {
	MachineID string    `json:"machine_id"`
	FailedAt  time.Time `json:"failed_at"`
}

// loadHWEStamp: a missing path or file is the zero stamp.
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

// keepHWEStamp writes are best-effort (warned): the stamp only saves time.
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
