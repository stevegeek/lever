package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Resources is the memory and CPU limits of one agent container, passed to
// scion as resources.limits (podman's --memory and --cpus). Unset fields get
// scion's defaults. Create-time only: scion stores them in the agent record
// and a resume reuses them.
type Resources struct {
	// Memory is the hard limit past which the container is OOM-killed, which
	// keeps one agent from taking the VM down.
	Memory string `yaml:"memory" json:",omitempty"`
	// CPU is in cores ("1.5") or millicores ("500m").
	CPU string `yaml:"cpu" json:",omitempty"`
}

func (r Resources) IsZero() bool { return r.Memory == "" && r.CPU == "" }

// MinAgentMemory: Claude Code alone idles at 250-500 MB, so a lower cap only
// produces an agent the OOM killer takes at its first turn.
const MinAgentMemory = 512 << 20

var (
	agentMemoryRE = regexp.MustCompile(`^([0-9]{1,9})(MiB|GiB|Mi|Gi)$`)
	agentCPURE    = regexp.MustCompile(`^([0-9]{1,4}(\.[0-9]{1,3})?|[0-9]{1,7}m)$`)
)

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
// scion's parser reads "Gi", not "GiB".
func (r Resources) ScionLimits() (memory, cpu string) {
	memory = r.Memory
	if strings.HasSuffix(memory, "iB") {
		memory = strings.TrimSuffix(memory, "B")
	}
	return memory, r.CPU
}

// validateResources: a limit larger than the VM is no limit, and almost
// always a unit mistake.
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

const MaxRunningWorkersLimit = 1000

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

// EffectiveGuestProfile is "" off Lima.
func (a *App) EffectiveGuestProfile() string {
	if a.Backend != BackendLima {
		return ""
	}
	if a.GuestProfile != "" {
		return a.GuestProfile
	}
	return GuestProfileStandard
}
