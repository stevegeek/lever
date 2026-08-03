package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/config"
)

// resolveConfigPath returns an explicit config path when given, otherwise the
// canonical config in the CURRENT directory only. There is deliberately NO
// walk-up discovery: run `lever` from the instance root. This prevents a
// `lever.yaml` planted in a parent directory from being picked up and trusted.
// See security-model-config-trust.md §5.
func resolveConfigPath(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	p := filepath.Join(wd, config.CanonicalName)
	if fi, statErr := os.Stat(p); statErr != nil || fi.IsDir() {
		return "", fmt.Errorf("no %s in the current directory (%s) — run lever from the instance root, or pass a config path", config.CanonicalName, wd)
	}
	return p, nil
}

// argOrEmpty returns args[0] if present, else "".
func argOrEmpty(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// machineName derives the jail machine name from an app name, matching
// buildApplyDeps so up/apply/down/doctor all agree on the same jail.
func machineName(appName string) string { return "lever-" + appName }

// machineFromFlagOrConfig returns the explicit --machine when set, else derives
// lever-<name> from the discovered canonical config. This makes `lever down` /
// `lever doctor` target the right jail when run inside an instance, instead of
// the generic default that never matches a real instance.
func machineFromFlagOrConfig(machine string) (string, error) {
	if machine != "" {
		return machine, nil
	}
	path, err := resolveConfigPath("")
	if err != nil {
		return "", fmt.Errorf("no --machine given and could not resolve a config: %w", err)
	}
	app, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return machineName(app.Name), nil
}

// addJailTargetFlags registers the --machine/--backend flag pair shared by the
// host-side jail-targeting commands (destroy/stop/doctor/worker purge) and
// returns pointers to the bound values. Pointers, not values: destroy/stop read
// the raw --machine flag before resolution (machine=="" gates their broker-stop
// branch).
func addJailTargetFlags(c *cobra.Command) (machine, backendFlag *string) {
	machine, backendFlag = new(string), new(string)
	c.Flags().StringVar(machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	c.Flags().StringVar(backendFlag, "backend", "", "containment backend (default: config's backend, else the registry default)")
	return machine, backendFlag
}

// resolveJailBackend resolves the jail machine name from the --machine flag (or
// config) and constructs the backend from the --backend flag (or config, or the
// registry default) — the resolution half of addJailTargetFlags.
func resolveJailBackend(factory BackendFactory, machine, backendFlag string) (string, backend.Backend, error) {
	m, err := machineFromFlagOrConfig(machine)
	if err != nil {
		return "", nil, err
	}
	b, err := factory(backendFromFlagOrConfig(backendFlag), m)
	if err != nil {
		return "", nil, err
	}
	return m, b, nil
}

// backendFromFlagOrConfig returns the explicit --backend when set, else the
// resolved config's backend, else the registry default (flag-only usage away
// from an instance root).
func backendFromFlagOrConfig(flag string) string {
	if flag != "" {
		return flag
	}
	if path, err := resolveConfigPath(""); err == nil {
		if app, err := config.Load(path); err == nil {
			return app.Backend
		}
	}
	return registry.Default
}
