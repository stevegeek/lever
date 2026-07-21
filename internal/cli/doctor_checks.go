package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
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

// checkToolBackends verifies every broker tool is reachable/spawnable up
// front: external tools must be listening on their loopback backend, and
// supervised tools must have their command resolvable on the supervisor PATH
// (a not-on-PATH supervised tool fails silently at spawn otherwise). Config
// validation already rejects an unresolvable supervised command, so a config
// that loaded is expected to pass the resolution half here — this check is the
// operator-facing confirmation and the external-liveness probe.
func checkToolBackends(tools []config.Tool, dial dialFunc) checkResult {
	const name = "tool backends"
	var down []string
	probed := 0
	for _, t := range tools {
		probed++
		if t.External {
			addr := backendHostPort(t.Backend)
			if err := dial(addr); err != nil {
				down = append(down, fmt.Sprintf("%s (external, %s)", t.Name, addr))
			}
			continue
		}
		if len(t.Command) > 0 {
			bin := t.Command[0]
			if !strings.ContainsRune(bin, '/') {
				if _, err := config.LookPathIn(bin, config.ToolSupervisorPATH); err != nil {
					down = append(down, fmt.Sprintf("%s (supervised, %q not on PATH)", t.Name, bin))
				}
			} else if !config.IsExecutableFile(bin) {
				down = append(down, fmt.Sprintf("%s (supervised, %q is not an executable file)", t.Name, bin))
			}
		}
	}
	switch {
	case probed == 0:
		return checkResult{name, true, "no tools declared", ""}
	case len(down) > 0:
		return checkResult{name, false, "unreachable: " + strings.Join(down, ", "), "start external server(s) on their loopback backend; for supervised tools, use an absolute command or install it on " + config.ToolSupervisorPATH}
	default:
		return checkResult{name, true, fmt.Sprintf("%d ok", probed), ""}
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
// are present, current for this lever version, and unmodified — or adopted as
// an accepted baseline via `lever init --adopt` — and referenced from the
// tree-root CLAUDE.md. Runs the scaffold engine in check (read-only) mode.
// Drift PAST an adopted baseline is called out separately: the scaffolds live
// inside the agent-writable tree, so unexplained change there is the tamper
// signal this check exists for.
func checkOperatorSkills(app *config.App, stateDir string) checkResult {
	const name = "operator skills"
	results, err := syncSkills(app, stateDir, false, true)
	if err != nil {
		return checkResult{name, false, "could not inspect skill scaffolds: " + err.Error(), "run `lever init`"}
	}
	blockAct, err := ensureClaudeMDBlock(app.Tree, stateDir, false, true)
	if err != nil {
		return checkResult{name, false, "could not inspect CLAUDE.md: " + err.Error(), "run `lever init`"}
	}
	if skillsUpToDate(results, blockAct) {
		nAdopted := 0
		for _, r := range results {
			if r.Action == skillAdopted {
				nAdopted++
			}
		}
		if nAdopted > 0 || blockAct == skillAdopted {
			blockDesc := "block present"
			if blockAct == skillAdopted {
				blockDesc = "adopted as custom"
			}
			return checkResult{name, true, fmt.Sprintf("%d scaffold(s) OK (%d adopted as custom), CLAUDE.md %s", len(results), nAdopted, blockDesc), ""}
		}
		return checkResult{name, true, fmt.Sprintf("%d scaffold(s) current (lever-operator + workers), CLAUDE.md block present", len(results)), ""}
	}
	adopted, err := loadAdoptedState(stateDir)
	if err != nil { // syncSkills already parsed it, so this is unreachable
		return checkResult{name, false, "could not inspect adopted baselines: " + err.Error(), "run `lever init --adopt`"}
	}
	var bad []string
	modified, adoptDrift := false, false
	for _, r := range results {
		if r.Action == skillUnchanged || r.Action == skillAdopted {
			continue
		}
		label := string(r.Action)
		if r.Action == skillSkipped {
			if _, ok := adopted[r.RelPath]; ok {
				label = "modified since adoption"
				adoptDrift = true
			} else {
				modified = true
			}
		}
		bad = append(bad, fmt.Sprintf("%s: %s", r.RelPath, label))
	}
	if blockAct != skillUnchanged && blockAct != skillAdopted {
		label := string(blockAct)
		if blockAct == skillSkipped { // only reachable via an adoption record
			label = "modified since adoption"
			adoptDrift = true
		}
		bad = append(bad, fmt.Sprintf("CLAUDE.md lever:skills block: %s", label))
	}
	fix := "run `lever init`"
	switch {
	case adoptDrift:
		fix = "changed since you adopted it — review the diff (an agent can edit files in the tree), then re-adopt with `lever init --adopt` or restore with `lever init --force`"
	case modified:
		fix = "locally-modified scaffold(s): if the edits are yours, accept them as your baseline with `lever init --adopt` (drift past it still fails this check); otherwise restore with `lever init --force`"
	}
	return checkResult{name, false, strings.Join(bad, "; "), fix}
}

// checkDirectives verifies the operator-directive channel is usable when
// configured. Directives are opt-in (gated solely by operator.allowed_signers,
// see config.App.DirectivesEnabled), so an unset config is a pass, not a
// warning: most instances never touch this feature. Once configured, three
// things can silently break the channel without any config-load error —
// allowed_signers missing/empty (nothing to verify a signature against),
// ssh-keygen absent from PATH (opsig shells out to it for both signing and
// verification), and — when the broker is actually up — a missing directive
// socket (serve.go creates it at startup; its absence means directives can't
// reach the broker even though everything else looks configured).
func checkDirectives(app *config.App, st brokerctl.State) checkResult {
	const name = "operator directives"
	if !app.DirectivesEnabled() {
		return checkResult{name, true, "not configured (operator.allowed_signers unset)", ""}
	}
	path := app.OperatorAllowedSignersPath()
	genHint := fmt.Sprintf("generate a key with `ssh-keygen -t ed25519 -f <keyfile>`, then add a line `%s <type> <keydata>` (from <keyfile>.pub) to %s", app.OperatorPrincipal(), path)
	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{name, false, fmt.Sprintf("allowed_signers %s: %s", path, err), genHint}
	}
	n := countKeyLines(data)
	if n == 0 {
		return checkResult{name, false, fmt.Sprintf("allowed_signers %s has no key lines", path), genHint}
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return checkResult{name, false, "ssh-keygen not found on PATH (directive signing/verification shells out to it)",
			"install the OpenSSH client tools so `ssh-keygen` resolves on PATH"}
	}
	_, found, alive := st.BrokerPIDStatus()
	if !found || !alive {
		return checkResult{name, true, fmt.Sprintf("allowed_signers: %d key(s); broker not running (socket check skipped)", n), ""}
	}
	if _, err := os.Stat(st.DirectiveSock()); err != nil {
		return checkResult{name, false, fmt.Sprintf("allowed_signers: %d key(s); broker is running but the directive socket %s is absent", n, st.DirectiveSock()),
			"restart the broker (`lever apply` or `lever up`) so it (re)creates the directive socket"}
	}
	return checkResult{name, true, fmt.Sprintf("allowed_signers: %d key(s); socket present", n), ""}
}

// countKeyLines counts substantive lines in an allowed_signers file: each
// holds "principal keytype keydata"; blank lines and #-comments don't count.
// Not a full ssh-keygen(1) allowed_signers parser (which also supports
// per-line options like cert-authority/namespaces) — doctor only needs a
// "is there at least one usable key" signal, not full validation (ssh-keygen
// itself is the source of truth when directives are actually verified).
func countKeyLines(data []byte) int {
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
	}
	return n
}

// certRejectWindow bounds how recently the broker must have rejected an expired
// leaf for checkAgentCert to treat it as an ACTIVE failure. Wider than the
// agent's handshake-retry cadence (seconds) so an ongoing outage always lands a
// match inside it, yet narrow enough that once a re-enrol heals the leaf the
// old log lines age out and the check self-clears.
const certRejectWindow = 15 * time.Minute

// brokerLogTailBytes caps how much of broker.out.log checkAgentCert reads (from
// the end): enough to cover a current outage, bounded so a long-lived log never
// costs a full read.
const brokerLogTailBytes = 64 << 10

// checkAgentCert reports whether the broker is CURRENTLY rejecting an agent's
// mTLS leaf as expired. The failure that motivates it — a short-lived agent leaf
// that lapses while the instance is down (the in-container renew sidecar can't
// run while stopped) — shows up ONLY as a TLS handshake error in the broker's
// own log; a host-side CA check reads green throughout it (the CA is long-lived,
// it's the leaf that died). So this scans broker.out.log for the exact
// fingerprint rather than inspecting any cert file.
func checkAgentCert(st brokerctl.State, now time.Time) checkResult {
	const name = "agent certificate"
	latest, found, err := scanBrokerLogCertExpiry(st.OutLog())
	switch {
	case err != nil:
		// No readable broker log (never started, or cleanly removed). A missing
		// broker is checkBrokerAlive's job; here there's nothing to diagnose.
		return checkResult{name, true, "no broker log to scan", ""}
	case !found:
		return checkResult{name, true, "no expired-leaf rejections in the broker log", ""}
	case now.Sub(latest) <= certRejectWindow:
		// A rejection logged before the CURRENT broker started (pid-file mtime)
		// describes the pre-restart outage, not this broker — the restart is
		// exactly the remedy, so don't cry wolf right after it heals.
		if start, ok := brokerStartTime(st.PID()); ok && latest.Before(start) {
			return checkResult{name, true,
				fmt.Sprintf("last expired-leaf rejection at %s predates the current broker (started %s) — healed by restart",
					latest.Format("2006-01-02 15:04:05"), start.Format("2006-01-02 15:04:05")), ""}
		}
		return checkResult{name, false,
			fmt.Sprintf("broker is rejecting an agent's mTLS leaf as expired (last at %s) — brokered tools are down", latest.Format("2006-01-02 15:04:05")),
			"run `lever up`: it stages a fresh enrolment ticket so the agent renews its expired leaf on boot (no teardown needed). If it persists, `lever destroy && lever up`"}
	default:
		return checkResult{name, true,
			fmt.Sprintf("last expired-leaf rejection at %s (stale, not currently failing)", latest.Format("2006-01-02 15:04:05")), ""}
	}
}

// brokerStartTime reports when the current broker started, via the pid file's
// mtime (written once, after the listeners bind). ok=false if there is no pid
// file (broker not running — checkBrokerAlive's job).
func brokerStartTime(pidPath string) (time.Time, bool) {
	fi, err := os.Stat(pidPath)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// scanBrokerLogCertExpiry returns the timestamp of the most recent expired- /
// bad-certificate TLS handshake error in the tail of the broker log at path.
// Lines are Go-stdlog-prefixed ("2006/01/02 15:04:05 …"); the prefix is parsed
// in local time (the broker logs local time). Lines without the fingerprint or
// without a parseable timestamp are ignored.
func scanBrokerLogCertExpiry(path string) (time.Time, bool, error) {
	data, err := readFileTail(path, brokerLogTailBytes)
	if err != nil {
		return time.Time{}, false, err
	}
	var latest time.Time
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "certificate has expired") && !strings.Contains(line, "tls: bad certificate") {
			continue
		}
		if len(line) < 19 {
			continue
		}
		ts, perr := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local)
		if perr != nil {
			continue
		}
		if !found || ts.After(latest) {
			latest, found = ts, true
		}
	}
	return latest, found, nil
}

// readFileTail returns up to the last max bytes of the file at path.
func readFileTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if off := fi.Size() - max; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
