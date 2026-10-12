package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Resources is manager.resources and workers[].resources: the memory and CPU
// LIMITS of one agent container. lever passes them to scion as the agent's
// resources.limits (scion ResourceSpec), which the runtime emits as podman's
// --memory and --cpus. Unset fields stay unset: scion then applies its own
// defaults (a CPU limit of 2 since the pin lever ships; no memory limit).
//
// Create-time only, like Model: scion stores the resolved resources in the
// agent record, and a resume reuses them. A changed value reaches an agent
// only on a fresh create (`lever up --fresh` for the manager, a purge or a
// recycle for a worker).
type Resources struct {
	// Memory is the hard memory limit, e.g. 1536MiB or 2GiB (Mi and Gi are
	// accepted too). At least MinAgentMemory; at most the VM's memory when
	// memory: is set. The container's processes are OOM-killed past it,
	// which keeps one agent from taking the VM (and the manager) down.
	Memory string `yaml:"memory" json:",omitempty"`
	// CPU is the CPU limit in cores: a decimal ("1", "1.5") or millicores
	// ("500m"). Write a decimal in quotes or not; YAML's number reads the
	// same. At most the VM's cpus: when that is set.
	CPU string `yaml:"cpu" json:",omitempty"`
}

// IsZero reports whether neither limit is set.
func (r Resources) IsZero() bool { return r.Memory == "" && r.CPU == "" }

// MinAgentMemory is the smallest memory limit lever accepts for an agent:
// Claude Code alone idles at 250-500 MB, so a lower cap only produces an
// agent the OOM killer takes at its first turn.
const MinAgentMemory = 512 << 20

var (
	agentMemoryRE = regexp.MustCompile(`^([0-9]{1,9})(MiB|GiB|Mi|Gi)$`)
	agentCPURE    = regexp.MustCompile(`^([0-9]{1,4}(\.[0-9]{1,3})?|[0-9]{1,7}m)$`)
)

// agentMemoryBytes parses a Resources.Memory value.
func agentMemoryBytes(s string) (int64, bool) {
	m := agentMemoryRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	if strings.HasPrefix(m[2], "G") {
		return n << 30, true
	}
	return n << 20, true
}

// agentCPUMillis parses a Resources.CPU value into millicores.
func agentCPUMillis(s string) (int64, bool) {
	if !agentCPURE.MatchString(s) {
		return 0, false
	}
	if strings.HasSuffix(s, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		return n, err == nil
	}
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	f := int64(0)
	if frac != "" {
		frac = (frac + "00")[:3]
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, false
		}
	}
	return w*1000 + f, true
}

// ScionLimits returns the limits in the form scion's ResourceList takes:
// memory with a Kubernetes binary suffix (scion's parser reads "Gi", not
// "GiB"), CPU as written. Empty for an unset field.
func (r Resources) ScionLimits() (memory, cpu string) {
	memory = r.Memory
	if strings.HasSuffix(memory, "iB") {
		memory = strings.TrimSuffix(memory, "B")
	}
	return memory, r.CPU
}

// validateResources checks one agent's resources against their own shape
// and, when the instance sizes its Lima VM, against the VM: a limit larger
// than the VM is no limit, and almost always a unit mistake.
func (a *App) validateResources(who string, r Resources) error {
	if r.Memory != "" {
		n, ok := agentMemoryBytes(r.Memory)
		if !ok {
			return fmt.Errorf("config: %s resources.memory %q invalid (want e.g. 1536MiB or 2GiB)", who, r.Memory)
		}
		if n < MinAgentMemory {
			return fmt.Errorf("config: %s resources.memory %q too small: at least 512MiB (Claude Code alone needs 250-500 MB)", who, r.Memory)
		}
		if a.Memory != "" {
			if vm, ok := sizeBytes(a.Memory); ok && n > vm {
				return fmt.Errorf("config: %s resources.memory %q is more than the VM's memory %q", who, r.Memory, a.Memory)
			}
		}
	}
	if r.CPU != "" {
		m, ok := agentCPUMillis(r.CPU)
		if !ok {
			return fmt.Errorf("config: %s resources.cpu %q invalid (want cores such as 1 or 1.5, or millicores such as 500m)", who, r.CPU)
		}
		if m <= 0 {
			return fmt.Errorf("config: %s resources.cpu %q must be more than 0", who, r.CPU)
		}
		if a.CPUs > 0 && m > int64(a.CPUs)*1000 {
			return fmt.Errorf("config: %s resources.cpu %q is more than the VM's cpus (%d)", who, r.CPU, a.CPUs)
		}
	}
	return nil
}

// MaxRunningWorkersLimit bounds broker.max_running_workers.
const MaxRunningWorkersLimit = 1000

// validateWorkerCap checks broker.max_running_workers.
func (a *App) validateWorkerCap() error {
	n := a.Broker.MaxRunningWorkers
	if n < 0 || n > MaxRunningWorkersLimit {
		return fmt.Errorf("config: broker.max_running_workers %d out of range (1-%d, or 0/unset for no cap)", n, MaxRunningWorkersLimit)
	}
	return nil
}

// Lima guest profiles (guest_profile).
const (
	GuestProfileStandard = "standard" // the stock Ubuntu cloud guest
	GuestProfileSmall    = "small"    // trimmed services and zram swap
)

// EffectiveGuestProfile is the guest profile apply converges a Lima guest
// on: guest_profile when set, else standard. "" off Lima.
func (a *App) EffectiveGuestProfile() string {
	if a.Backend != BackendLima {
		return ""
	}
	if a.GuestProfile != "" {
		return a.GuestProfile
	}
	return GuestProfileStandard
}
