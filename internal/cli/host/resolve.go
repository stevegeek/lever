package host

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
// See docs-site/_guides/security-model-config-trust.md §5.
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

// loadAppPath resolves the config path (an optional trailing CONFIG arg) and
// loads the App, returning both — the path is what the broker and remote
// proxy are re-exec'd with, and what the state dir hangs off. This is the one
// config load an apply-shaped command makes.
func loadAppPath(args []string) (string, *config.App, error) {
	path, err := resolveConfigPath(argOrEmpty(args))
	if err != nil {
		return "", nil, err
	}
	app, err := config.Load(path)
	if err != nil {
		return "", nil, err
	}
	return path, app, nil
}

// instanceApp is the canonical config in the current directory, loaded at most
// once per command, or the reason there is none. The jail-targeting commands
// (destroy/stop/doctor/worker purge) run with or without one: --machine and
// --backend stand in for it away from an instance root.
type instanceApp struct {
	path string      // "" when no config resolved
	app  *config.App // nil when err != nil
	err  error       // why app is nil
}

// loadInstanceApp resolves and loads the current directory's config; it never
// fails, it records why it could not instead.
func loadInstanceApp() instanceApp {
	path, err := resolveConfigPath("")
	if err != nil {
		return instanceApp{err: err}
	}
	app, err := config.Load(path)
	if err != nil {
		return instanceApp{path: path, err: err}
	}
	return instanceApp{path: path, app: app}
}

// machine returns the explicit --machine when set, else derives lever-<name>
// from the config. This makes `lever down` / `lever doctor` target the right
// jail when run inside an instance, instead of the generic default that never
// matches a real instance.
func (ia instanceApp) machine(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if ia.app == nil {
		if ia.path == "" {
			return "", fmt.Errorf("no --machine given and could not resolve a config: %w", ia.err)
		}
		return "", ia.err
	}
	return machineName(ia.app.Name), nil
}

// backend returns the explicit --backend when set, else the config's backend,
// else the registry default (flag-only usage away from an instance root).
func (ia instanceApp) backend(flag string) string {
	if flag != "" {
		return flag
	}
	if ia.app != nil {
		return ia.app.Backend
	}
	return registry.Default
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

// resolveJailBackend loads the current directory's config once and resolves
// the jail target from it — see resolveJailBackendFor.
func resolveJailBackend(factory BackendFactory, machine, backendFlag string) (string, backend.Backend, error) {
	return resolveJailBackendFor(factory, loadInstanceApp(), machine, backendFlag)
}

// resolveJailBackendFor resolves the jail machine name from the --machine flag
// (or ia's config) and constructs the backend from the --backend flag (or the
// config, or the registry default) — the resolution half of addJailTargetFlags.
func resolveJailBackendFor(factory BackendFactory, ia instanceApp, machine, backendFlag string) (string, backend.Backend, error) {
	m, err := ia.machine(machine)
	if err != nil {
		return "", nil, err
	}
	b, err := factory(ia.backend(backendFlag), m)
	if err != nil {
		return "", nil, err
	}
	return m, b, nil
}
