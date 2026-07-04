package cli

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
)

// checkResult is one diagnostic outcome. detail is shown in both the pass and
// fail lines; fix is a remediation hint shown only on failure.
type checkResult struct {
	name   string
	ok     bool
	detail string
	fix    string
}

// dialFunc probes a TCP address, returning nil if something is listening. It is
// injected so the checks are unit-testable without real listeners.
type dialFunc func(addr string) error

// tcpDial is the production dialFunc: a short-timeout TCP connect, closed at once.
func tcpDial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// checkBrokerAlive verifies the recorded broker process is alive AND actually
// listening on the jail port. It distinguishes three failure modes so the fix
// is unambiguous: never started, died (stale pid), and alive-but-not-serving.
func checkBrokerAlive(st brokerctl.State, jailPort int, dial dialFunc) checkResult {
	const name = "broker running"
	pid, found, alive := st.BrokerPIDStatus()
	switch {
	case !found:
		return checkResult{name, false, "no broker.pid — the broker was never started (or was cleanly stopped)", "run `lever apply` or `lever up`"}
	case !alive:
		return checkResult{name, false, fmt.Sprintf("broker.pid names pid %d, but that process is gone (stale pid file)", pid), "run `lever apply` or `lever up`"}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", jailPort)
	if err := dial(addr); err != nil {
		return checkResult{name, false, fmt.Sprintf("pid %d is alive but nothing is listening on %s", pid, addr), "inspect .lever-state/broker.log, then restart with `lever apply`"}
	}
	return checkResult{name, true, fmt.Sprintf("pid %d, serving on %s", pid, addr), ""}
}

// checkExternalBackends verifies every external tool's backend is listening.
// External servers are fronted, not spawned, so a down one surfaces only as a
// gateway 502 at call time — this turns that into an up-front diagnosis.
func checkExternalBackends(tools []config.Tool, dial dialFunc) checkResult {
	const name = "external tool backends"
	var down []string
	probed := 0
	for _, t := range tools {
		if !t.External {
			continue
		}
		probed++
		addr := backendHostPort(t.Backend)
		if err := dial(addr); err != nil {
			down = append(down, fmt.Sprintf("%s (%s)", t.Name, addr))
		}
	}
	switch {
	case probed == 0:
		return checkResult{name, true, "no external tools declared", ""}
	case len(down) > 0:
		return checkResult{name, false, "not listening: " + strings.Join(down, ", "), "start the server(s) (e.g. your MCP launcher); each must listen on its loopback backend"}
	default:
		return checkResult{name, true, fmt.Sprintf("%d reachable", probed), ""}
	}
}

// checkScionProject flags the bad-teardown corruption: scion has registered the
// tree (a ~/.scion/project-configs entry whose workspace_path is the mount dest)
// but the in-tree marker is gone, or there are duplicate registrations for it.
// Either state makes `scion init` fail with "existing project marker is invalid",
// blocking the manager from coming up. A pure function over the state the backend
// read, so it is testable without a jail.
func checkScionProject(st backend.ScionProjectState, mountDest string) checkResult {
	const name = "scion project registration"
	var reg []string
	for _, e := range st.Entries {
		if e.WorkspacePath == mountDest {
			reg = append(reg, e.Name)
		}
	}
	switch {
	case len(reg) == 0:
		return checkResult{name, true, "no stale registration for " + mountDest, ""}
	case !st.MarkerPresent:
		return checkResult{name, false,
			fmt.Sprintf("scion is registered for %s (%s) but the in-tree %s/.scion marker is gone — the signature of a bad teardown (a bare container kill instead of scion suspend/down)", mountDest, strings.Join(reg, ", "), mountDest),
			fmt.Sprintf("in the jail, remove the stale registration(s) ~/.scion/project-configs/%s then run `lever apply`", braceList(reg))}
	case len(reg) > 1:
		return checkResult{name, false,
			fmt.Sprintf("scion has %d duplicate registrations for %s (%s)", len(reg), mountDest, strings.Join(reg, ", ")),
			fmt.Sprintf("in the jail, keep one and remove the rest under ~/.scion/project-configs/%s then run `lever apply`", braceList(reg))}
	default:
		return checkResult{name, true, "consistent (" + reg[0] + ")", ""}
	}
}

// braceList renders names as a shell brace-expansion hint ({a,b}) for the fix
// text, or the bare name for a single entry.
func braceList(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return "{" + strings.Join(names, ",") + "}"
}

// backendHostPort strips an optional path from a "host:port[/path]" backend,
// leaving the "host:port" a TCP dial needs. External backends are validated
// scheme-less, so no scheme handling is required.
func backendHostPort(backend string) string {
	if i := strings.IndexByte(backend, '/'); i >= 0 {
		return backend[:i]
	}
	return backend
}

// checkCredentialFile verifies the subscription credential apply's credential
// step will read: present, non-empty, and not group/other-accessible. The
// detail reports size and mode ONLY — never file contents. An unset path is a
// pass: api-key instances have no credential_file.
func checkCredentialFile(path string) checkResult {
	const name = "manager credential"
	const mint = "mint one with `claude setup-token`, save it to the configured path, then chmod 600 it"
	if path == "" {
		return checkResult{name, true, "no credential_file configured", ""}
	}
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return checkResult{name, false, path + " is missing", mint}
	case fi.Size() == 0:
		return checkResult{name, false, path + " is empty", mint}
	case fi.Mode().Perm()&0o077 != 0:
		return checkResult{name, false, fmt.Sprintf("%s has mode %04o (group/other-accessible)", path, fi.Mode().Perm()), "chmod 600 " + path}
	default:
		return checkResult{name, true, fmt.Sprintf("%s (%d bytes, mode %04o)", path, fi.Size(), fi.Mode().Perm()), ""}
	}
}

// checkMcpJsonInTree flags any .mcp.json anywhere under the host tree.
// Claude auto-loads a .mcp.json as PROJECT scope inside every jailed agent,
// which collides with the brokered USER-scope tools lever-agent registers
// (duplicate localhost:PORT endpoints vs the broker's) — a real bug hit in
// production. Walks the whole tree (not just the top level); unreadable
// directories are skipped rather than failing the check outright.
func checkMcpJsonInTree(tree string) checkResult {
	const name = "no stray .mcp.json in tree"
	var found []string
	_ = filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry (permissions, race): skip it, don't abort the walk.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && d.Name() == ".mcp.json" {
			found = append(found, path)
		}
		return nil
	})
	if len(found) > 0 {
		return checkResult{name, false, "found: " + strings.Join(found, ", "),
			"remove it — brokered MCP tools are registered at user scope by lever-agent; a .mcp.json in the tree re-adds ambient project-scope endpoints and conflicts"}
	}
	return checkResult{name, true, "none in tree", ""}
}

// goVersionProbe resolves and runs `go version` on the host PATH. It is a
// package-level var so tests can inject a fake outcome (mirrors dialFunc).
// The production implementation distinguishes "not on PATH at all" from "on
// PATH but broken" (e.g. a dead asdf/mise shim, which typically fails with
// exit status 126) by resolving via exec.LookPath first.
var goVersionProbe = func() (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, goBin, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s version: %w", goBin, err)
	}
	return string(out), nil
}

// checkGoToolchain verifies a real, working Go toolchain is resolvable on
// PATH when scion needs to be cross-compiled (source checkout or a pinned
// module version) — `lever up`/`apply` shell out to `go` for that build. A
// broken shim (e.g. asdf/mise without the version installed) fails with an
// opaque "exit status 126" deep inside apply; this turns it into an
// up-front, actionable diagnosis. No build requested => no go needed => pass.
func checkGoToolchain(scion config.ScionConfig) checkResult {
	const name = "go toolchain"
	if scion.Source == "" && scion.Version == "" {
		return checkResult{name, true, "scion build not required", ""}
	}
	out, err := goVersionProbe()
	if err != nil {
		return checkResult{name, false, "go toolchain not usable: " + err.Error(),
			`put a REAL Go toolchain on PATH (not just an asdf/mise shim), e.g. export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"; ` + "`go version` should print"}
	}
	return checkResult{name, true, strings.TrimSpace(out), ""}
}

// checkOperatorSkills verifies the framework skills scaffolded by `lever init`
// are present, current for this lever version, unmodified, and referenced from
// the tree-root CLAUDE.md. Runs the scaffold engine in check (read-only) mode.
func checkOperatorSkills(app *config.App, stateDir string) checkResult {
	const name = "operator skills"
	results, err := syncSkills(app, stateDir, false, true)
	if err != nil {
		return checkResult{name, false, "could not inspect skill scaffolds: " + err.Error(), "run `lever init`"}
	}
	blockAct, err := ensureClaudeMDBlock(app.Tree, true)
	if err != nil {
		return checkResult{name, false, "could not inspect CLAUDE.md: " + err.Error(), "run `lever init`"}
	}
	if skillsUpToDate(results, blockAct) {
		return checkResult{name, true, fmt.Sprintf("%d scaffold(s) current (lever-operator + groves), CLAUDE.md block present", len(results)), ""}
	}
	var bad []string
	modified := false
	for _, r := range results {
		if r.Action != skillUnchanged {
			bad = append(bad, fmt.Sprintf("%s: %s", r.RelPath, r.Action))
			if r.Action == skillSkipped {
				modified = true
			}
		}
	}
	if blockAct != skillUnchanged {
		bad = append(bad, fmt.Sprintf("CLAUDE.md lever:skills block: %s", blockAct))
	}
	fix := "run `lever init`"
	if modified {
		fix = "locally-modified scaffold(s): review, then `lever init --force` to overwrite (or keep your edits — this check stays informational)"
	}
	return checkResult{name, false, strings.Join(bad, "; "), fix}
}
