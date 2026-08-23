// Package lima implements the Backend contract via a Lima VM (own kernel) +
// rootless Docker + iptables egress.
//
// The prefix/guest-delegating half of the contract (DockerHost, the jail
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
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
)

// hostAlias is the DNS name Lima resolves to the host from inside the VM.
const hostAlias = "host.lima.internal"

// limaVersionRe matches "limactl version 2.1.3" lines from `limactl --version`.
var limaVersionRe = regexp.MustCompile(`limactl version (\d+)\.(\d+)\.(\d+)`)

type Lima struct {
	common.Base
}

func New(r proc.Runner, vm string) *Lima {
	l := &Lima{}
	l.Base = common.NewBase(common.Config{
		Runner:    r,
		Machine:   vm,
		HostAlias: hostAlias,
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
		},
	})
	return l
}

// JailPrefix is the argv prefix that executes inside this backend's VM.
// Exported for registry.JailRunner (broker-side re-derivation).
func JailPrefix(vm string) []string { return []string{"limactl", "shell", vm} }

// Profile returns lima's declared guarantees. The value lives once in
// backend.Candidates (the single source of the guarantee matrix); returning it
// here keeps the runtime profile and the documented one identical.
func (l *Lima) Profile() backend.Profile {
	p, _ := backend.ProfileFor("lima")
	return p
}

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
	if err := l.ensureVM(ctx, cfg.ProjectTree, cfg.Disk); err != nil {
		return err
	}
	if err := l.ReadRunUser(ctx); err != nil {
		return err
	}
	if err := l.Guest().EnsureRuntimes(ctx, l.RunUser()); err != nil {
		return err
	}
	if cfg.HasScion() {
		if err := l.Guest().EnsureScion(ctx, guest.ScionSpec{
			Binary:  cfg.ScionBinary,
			Source:  cfg.ScionSource,
			Version: cfg.ScionVersion,
			WebUI:   cfg.ScionWebUI,
		}); err != nil {
			return err
		}
	}
	return l.ApplyEgress(ctx, cfg.AllowedPorts, cfg.ClosedInternet)
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
	if _, err := l.Runner().Run(ctx, nil, "limactl", "start", "--tty=false", l.Machine()); err != nil {
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
// template's intent for projectTree.
func (l *Lima) verifyRealizedConfig(ctx context.Context, projectTree string) error {
	res, err := l.Runner().Run(ctx, nil, "limactl", "list", "--json", l.Machine())
	if err != nil {
		return fmt.Errorf("read back realized config for %q: %w", l.Machine(), err)
	}
	var inst realizedInstance
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &inst); err != nil {
		return fmt.Errorf("parse realized config for %q: %w", l.Machine(), err)
	}
	if !inst.matchesContainment(projectTree) {
		return fmt.Errorf("lima VM %q exists with a mismatched containment config (mounts/port-forwards/containerd drifted from the lever template); run 'lever down' then 'lever up' to recreate", l.Machine())
	}
	return nil
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

var _ backend.Backend = (*Lima)(nil)
