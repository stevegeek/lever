package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/provision/loginfwd"
)

// ToolSupervisorPATH is the exact PATH the broker supervisor spawns command
// tools with; config validation resolves against the same string so a
// missing command fails at config load, not at spawn time.
const ToolSupervisorPATH = "/usr/local/bin:/usr/bin:/bin"

// IsExecutableFile reports whether p is a non-directory with an executable
// bit set.
func IsExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// LookPathIn resolves bin against pathList, independent of the process
// environment.
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

// CheckHost runs the probes Validate leaves out because they read the host.
func (a *App) CheckHost() error {
	if err := a.checkNonGitTree(); err != nil {
		return err
	}
	for _, t := range a.Broker.Tools {
		if err := t.checkHost(a.InstanceDir()); err != nil {
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

// ResolveToolCommand is the program a supervised tool's command names, as
// the supervisor executes it: a bare name is looked up on ToolSupervisorPATH,
// never the caller's own PATH, and a relative path is joined onto dir.
func ResolveToolCommand(bin, dir string) (string, error) {
	switch {
	case !strings.ContainsRune(bin, '/'):
		return LookPathIn(bin, ToolSupervisorPATH)
	case filepath.IsAbs(bin):
		return bin, nil
	default:
		return filepath.Join(dir, bin), nil
	}
}

// InstanceDir is the instance root (the config file's directory), absolute.
// It is every supervised tool's working directory, so a relative path in a
// tool command means the same to the tree check as to the tool.
func (a *App) InstanceDir() string {
	if abs, err := filepath.Abs(a.dir); err == nil {
		return abs
	}
	return a.dir
}

func (t Tool) checkHost(dir string) error {
	if t.External || len(t.Command) == 0 {
		return nil
	}
	bin := t.Command[0]
	p, err := ResolveToolCommand(bin, dir)
	if err != nil {
		return fmt.Errorf("config: broker tool %q command %q not found on the supervisor PATH (%s); use an absolute path or install it there", t.Name, bin, ToolSupervisorPATH)
	}
	if !IsExecutableFile(p) {
		return fmt.Errorf("config: broker tool %q command %q is not an executable file", t.Name, bin)
	}
	return nil
}

// checkAPIKeyFile requires exactly 0600, failing closed on a group- or
// world-readable key.
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

// checkNonGitTree refuses a tree that is itself a git repository: sibling
// isolation assumes a non-git tree. Ancestors are not checked: scion mounts
// the exact tree dir, so an ancestor .git is never exposed.
func (a *App) checkNonGitTree() error {
	if treeIsGitRepo(a.Tree) {
		return fmt.Errorf("config: tree %q is itself a git repository; lever targets non-git trees "+
			"(per-worker git workflows are deferred, spec §13). A plain subdirectory inside a larger "+
			"git repo is fine — point tree at a non-git directory (or a subdir) instead", a.Tree)
	}
	return nil
}

// remoteForwarderPrebuilt is a variable so tests do not depend on how the
// test binary was built.
var remoteForwarderPrebuilt = loginfwd.PrebuiltComplete

// checkRemoteToolchain: a lever built without the embedded guest login
// forwarder cross-compiles it at apply time and needs Go. Only
// `scion.binary:` is checked, the one mode that needs no Go for scion
// itself. It runs at config load because apply would only find out after it
// has opened a mint window and touched the hub.
func (a *App) checkRemoteToolchain() error {
	if !a.Remote.Enabled || a.Scion.Binary == "" || remoteForwarderPrebuilt() {
		return nil
	}
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("config: remote: this lever carries no prebuilt guest login forwarder, so remote access " +
			"cross-compiles it at apply time and needs a Go toolchain — but `go` is not on PATH. Use a release build of " +
			"lever (or build it with `make install`, which embeds the forwarder), install Go (a REAL go on PATH, not just " +
			"an asdf/mise shim), or set remote.enabled: false")
	}
	return nil
}
