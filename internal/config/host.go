package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// CheckHost runs the probes Validate deliberately leaves out because they
// read the host: the tree's own .git, each supervised tool's binary, the
// api_key_file's mode, and the Go toolchain remote access needs. Load runs it
// after Validate; LoadNoHostChecks skips it for callers (tests, offline
// inspection) that only need the shape.
func (a *App) CheckHost() error {
	if err := a.checkNonGitTree(); err != nil {
		return err
	}
	for _, t := range a.Broker.Tools {
		if err := t.checkHost(); err != nil {
			return err
		}
	}
	if a.AnyAPIKeyAgent() {
		if err := checkAPIKeyFile(a.Broker.APIKeyFile); err != nil {
			return err
		}
	}
	return a.checkRemoteToolchain()
}

// checkHost verifies a supervised tool's command is spawnable on the
// supervisor's fixed PATH (or is an executable file when given as a path).
func (t Tool) checkHost() error {
	if t.External || len(t.Command) == 0 {
		return nil
	}
	bin := t.Command[0]
	if !strings.ContainsRune(bin, '/') {
		if _, err := LookPathIn(bin, ToolSupervisorPATH); err != nil {
			return fmt.Errorf("config: broker tool %q command %q not found on the supervisor PATH (%s); use an absolute path or install it there", t.Name, bin, ToolSupervisorPATH)
		}
	} else if !IsExecutableFile(bin) {
		return fmt.Errorf("config: broker tool %q command %q is not an executable file", t.Name, bin)
	}
	return nil
}

// checkAPIKeyFile requires the api-key file to exist at exactly 0600 (fail
// closed on a world/group-readable key).
func checkAPIKeyFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config: broker.api_key_file %q: %w", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return fmt.Errorf("config: broker.api_key_file %q must be 0600, got %#o", path, perm)
	}
	return nil
}

// checkNonGitTree refuses a tree that is ITSELF a git repository (has its
// own .git). R4's sibling isolation assumes a non-git tree; per-worker git
// workflows are deferred (spec §13). It deliberately does not walk up to
// ancestors: the pinned Scion's --workspace guard plain-mounts
// the exact tree dir, so an ancestor .git elsewhere in the checkout is never
// exposed and is harmless — a plain subdirectory inside a larger git repo is
// fine.
func (a *App) checkNonGitTree() error {
	if treeIsGitRepo(a.Tree) {
		return fmt.Errorf("config: tree %q is itself a git repository; lever targets non-git trees "+
			"(per-worker git workflows are deferred, spec §13). A plain subdirectory inside a larger "+
			"git repo is fine — point tree at a non-git directory (or a subdir) instead", a.Tree)
	}
	return nil
}

// checkRemoteToolchain: remote access needs a Go toolchain on the host — the
// guest-side login forwarder is cross-compiled for the guest's architecture
// at apply time (internal/backend/guest.EnsureHubLogin).
//
// Only `scion.binary:` is checked here, because that is the mode this
// requirement is NEW for — the other two already need Go to build scion
// itself, and their missing-toolchain diagnosis lives in `lever doctor`
// and in the build's own failure. Checking at config load rather than
// during apply is deliberate: EnsureHubLogin runs in the scion-server
// step, well after the bootstrap-token step has opened a mint window and
// touched the hub, and "your host has no compiler" is not something to
// discover half way through that.
func (a *App) checkRemoteToolchain() error {
	if a.Remote.Enabled && a.Scion.Binary != "" {
		if _, err := exec.LookPath("go"); err != nil {
			return fmt.Errorf("config: remote: remote access needs a Go toolchain on this host — it cross-compiles the " +
				"guest's login forwarder at apply time — but `go` is not on PATH. Install Go (or put a REAL go on PATH, " +
				"not just an asdf/mise shim), or set remote.enabled: false")
		}
	}
	return nil
}
