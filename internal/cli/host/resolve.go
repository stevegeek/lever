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

// resolveConfigPath returns arg when given, else the canonical config in the
// current directory only. There is deliberately no walk-up discovery, so a
// lever.yaml planted in a parent directory is never trusted.
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

func argOrEmpty(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// machineName must match buildApplyDeps so every command targets the same jail.
func machineName(appName string) string { return "lever-" + appName }

// loadAppPath resolves the config path (an optional trailing CONFIG arg) and
// loads the App. The path is what the broker and proxy are re-exec'd with and
// what the state dir hangs off.
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

// instanceApp is the current directory's config, loaded at most once, or why
// there is none. The jail-targeting commands also run without one, with
// --machine and --backend.
type instanceApp struct {
	path string      // "" when no config resolved
	app  *config.App // nil when err != nil
	err  error       // why app is nil
}

// loadInstanceApp never fails; it records why it could not load instead.
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

// machine returns --machine when set, else lever-<name> from the config.
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

// backend returns --backend when set, else the config's, else the registry
// default.
func (ia instanceApp) backend(flag string) string {
	if flag != "" {
		return flag
	}
	if ia.app != nil {
		return ia.app.Backend
	}
	return registry.Default
}

// addJailTargetFlags registers --machine/--backend for the jail-targeting
// commands. It returns pointers because destroy and stop read the raw
// --machine (empty gates their broker stop).
func addJailTargetFlags(c *cobra.Command) (machine, backendFlag *string) {
	machine, backendFlag = new(string), new(string)
	c.Flags().StringVar(machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	c.Flags().StringVar(backendFlag, "backend", "", "containment backend (default: config's backend, else the registry default)")
	return machine, backendFlag
}

func resolveJailBackend(factory BackendFactory, machine, backendFlag string) (string, backend.Backend, error) {
	return resolveJailBackendFor(factory, loadInstanceApp(), machine, backendFlag)
}

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
