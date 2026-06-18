package cli

import (
	"fmt"
	"os"

	"github.com/lever-to/lever/internal/config"
)

// resolveConfigPath returns an explicit config path when given, otherwise
// discovers the canonical config by walking up from the current directory
// (git/npm/cargo-style). This is what lets `lever up` run with no argument from
// anywhere inside an instance.
func resolveConfigPath(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return config.FindConfig(wd)
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
