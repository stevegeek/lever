package cli

import (
	"fmt"
	"os"
	"path/filepath"

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
