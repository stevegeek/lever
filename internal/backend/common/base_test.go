package common

import (
	"slices"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

func newTestBase(opts Options) Base {
	return NewBase(Config{
		Runner:  proc.NewFakeRunner(),
		Machine: "m",
		Hooks: Hooks{
			JailPrefix: func(machine, _ string) []string { return []string{"enter", machine} },
		},
		Options: opts,
	})
}

// TestForceHostNetworkComesFromOptionsNotEnv pins that Base takes the
// host-network escape hatch from Options only: the environment is read once,
// by the registry, at construction.
func TestForceHostNetworkComesFromOptionsNotEnv(t *testing.T) {
	const marker = "SCION_FORCE_HOST_NETWORK=1"
	t.Setenv(jail.ForceHostNetworkEnv, "1")
	b := newTestBase(Options{})
	if slices.Contains(b.AttachArgv([]string{"true"}), marker) {
		t.Fatalf("Base read %s from the environment; argv = %v", jail.ForceHostNetworkEnv, b.AttachArgv([]string{"true"}))
	}

	t.Setenv(jail.ForceHostNetworkEnv, "0")
	b = newTestBase(Options{ForceHostNetwork: true})
	if !slices.Contains(b.AttachArgv([]string{"true"}), marker) {
		t.Fatalf("Options.ForceHostNetwork not applied; argv = %v", b.AttachArgv([]string{"true"}))
	}
}
