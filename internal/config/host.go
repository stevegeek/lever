package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ToolSupervisorPATH is the EXACT PATH the broker supervisor spawns command
// tools with (brokerctl.Supervisor). Config validation resolves each
// supervised tool's binary against this same string so a not-on-PATH command
// is rejected loudly at config-load instead of failing opaquely (or silently)
// at spawn time.
const ToolSupervisorPATH = "/usr/local/bin:/usr/bin:/bin"

// IsExecutableFile reports whether p is a regular file with at least one
// executable bit set — the same shape check LookPathIn applies to each PATH
// candidate, exported so a resolved/absolute supervised-tool command can be
// re-verified (config validation, and doctor's spawnability probe) without
// duplicating the stat logic.
func IsExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// LookPathIn resolves bin against an explicit colon-separated path list,
// independent of the process environment (mirrors the supervisor's fixed PATH).
func LookPathIn(bin, pathList string) (string, error) {
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, bin)
		if IsExecutableFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q not found in %q", bin, pathList)
}
