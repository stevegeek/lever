package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/config"
	leverexec "github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/jail"
	"github.com/spf13/cobra"
)

// acceptanceCheckNames returns the six acceptance checks in a fixed, documented order. The
// order is load-bearing: formatNote iterates this slice (NOT map order) so the
// emitted note is deterministic.
func acceptanceCheckNames() []string {
	return []string{
		"delegated-read", // worker CAN read its delegated, filtered rows
		"no-table-c",     // worker CANNOT read a scope outside the envelope
		"no-drop-filter", // worker CANNOT widen by dropping the attenuated filter
		"no-self-path",   // worker CANNOT self-mint an un-granted capability
		"egress-refused", // jail reaches the allowlisted broker port but NOT the non-allowlisted admin port
		"revocation",     // after revoke/bump-epoch the next call is denied
	}
}

// overallPass is true iff every named acceptance check passed. A missing entry counts
// as a failure (a check that never ran cannot pass the gate).
func overallPass(results map[string]bool) bool {
	for _, name := range acceptanceCheckNames() {
		if !results[name] {
			return false
		}
	}
	return true
}

// formatNote renders a dated markdown acceptance note. It iterates
// acceptanceCheckNames() (NOT map order) so output is byte-stable for a given
// results map, and declares an overall PASS/FAIL verdict that fails if ANY check
// fails or is missing.
func formatNote(results map[string]bool, date string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# acceptance — %s\n\n", date)
	fmt.Fprintf(&b, "Live real-jail capability gate (`lever acceptance`).\n\n")
	for _, name := range acceptanceCheckNames() {
		verdict := "FAIL"
		if results[name] {
			verdict = "PASS"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", verdict, name)
	}
	overall := "FAIL"
	if overallPass(results) {
		overall = "PASS"
	}
	fmt.Fprintf(&b, "\nOverall: %s\n", overall)
	return b.String()
}

func newAcceptanceCmd(bf BackendFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "acceptance [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring up a real jail and drive the six acceptance capability checks (live gate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			return runAcceptance(cmd.Context(), cmd, app, path, bf)
		},
	}
}

// runAcceptance is the live acceptance gate. It brings a REAL jail up (manager +
// executor grove `worker` + the `db` tool; manager delegate db.read → worker;
// worker empty obtain), drives the six checks inside the running jail, writes a
// dated note to docs/acceptance/<date>-acceptance.md, and returns a non-nil error if
// ANY check fails (so the command exits non-zero — the gate).
//
// LIVE/INTERFACE-LEVEL: the per-check closures are validated by running this on
// a machine with OrbStack, not in CI (CI covers acceptanceCheckNames + formatNote
// + the wiring). Each closure ACTUALLY attempts its operation against the live
// broker via the in-jail `lever-agent` CLI; a check that cannot yet be driven
// records a failure, never a vacuous pass.
func runAcceptance(ctx context.Context, cmd *cobra.Command, app *config.App, configPath string, bf BackendFactory) error {
	// 1. Bring the real jail up BROKER-ONLY: jail-up (machine + egress allowlist),
	//    broker-up (host broker + tools), mint-manager-bootstrap. The VM-level gate
	//    drives lever-agent directly and never invokes scion, so all the
	//    scion/container/registration steps (incl. init-machine, which needs a
	//    scion binary the fresh machine lacks) are omitted. The bootstrap step
	//    still deposits the manager bootstrap at <mount>/.lever/bootstrap.json.
	deps, ob, _, err := buildApplyDeps(ctx, app, configPath, bf)
	if err != nil {
		return fmt.Errorf("acceptance: bring-up deps: %w", err)
	}
	deps.BrokerOnly = true
	if err := apply.Run(ctx, app, deps); err != nil {
		return fmt.Errorf("acceptance: apply: %w", err)
	}

	// 2. A jail Runner execs `lever-agent` INSIDE the jail machine (where the
	//    agent identities live). The worker's delegated token is minted by the
	//    manager (delegate db.read → worker) and exercised here from the worker's
	//    identity directory. Identity dirs are VM-writable (vmIDDir), not the old
	//    hardcoded /home/{manager,worker}/.lever-id (those users don't exist in
	//    the broker-only VM — no agent containers were started).
	machine := machineName(app.Name)
	jr := jail.New(leverexec.RealRunner{}, jail.OrbPrefix(machine, ob.RunUser()), ob.RunUID())
	h := &acceptanceHarness{
		app:       app,
		jr:        jr,
		hostAlias: ob.HostToolAlias(),
		bootDir:   bootstrapDirInJail(app, ob.MountDest()),
		managerID: vmIDDir("manager"),
		workerID:  vmIDDir("worker"),
	}

	// 2b. Setup phase: install lever-agent in the VM, then enrol the manager and
	//     provision+enrol the worker. FAIL-CLOSED — any setup error aborts the
	//     gate with a clear message (never a vacuous check pass).
	if err := h.setup(ctx, machine); err != nil {
		return fmt.Errorf("acceptance: setup: %w", err)
	}

	results := map[string]bool{}
	for _, name := range acceptanceCheckNames() {
		ok, derr := h.run(ctx, name)
		results[name] = ok
		if derr != nil {
			cmd.PrintErrf("check %-14s error: %v\n", name, derr)
		}
		status := "FAIL"
		if ok {
			status = "PASS"
		}
		cmd.Printf("check %-14s %s\n", name, status)
	}

	// 3. Write the dated note.
	date := time.Now().Format("2006-01-02")
	note := formatNote(results, date)
	// Record the note beside the instance root (the config file's directory), so
	// it lives with the instance rather than inside the agent-writable tree.
	noteDir := filepath.Join(filepath.Dir(configPath), "docs", "acceptance")
	if err := os.MkdirAll(noteDir, 0o755); err != nil {
		return fmt.Errorf("acceptance: mkdir note dir: %w", err)
	}
	notePath := filepath.Join(noteDir, date+"-acceptance.md")
	if err := os.WriteFile(notePath, []byte(note), 0o644); err != nil {
		return fmt.Errorf("acceptance: write note: %w", err)
	}
	cmd.Printf("acceptance note: %s\n", notePath)

	// 4. The gate: any failing/missing check fails the command.
	if !overallPass(results) {
		return fmt.Errorf("acceptance: acceptance gate FAILED — see %s", notePath)
	}
	cmd.Println("acceptance PASSED")
	return nil
}

// bootstrapDirInJail returns the in-jail directory holding the manager's
// bootstrap.json (mint-manager-bootstrap writes <tree>/.lever/bootstrap.json,
// bind-mounted at <mount>/.lever inside the jail). lever-agent reads the broker
// URL + CA from it.
func bootstrapDirInJail(app *config.App, mount string) string {
	if mount == "" {
		return filepath.Join(app.Tree, ".lever")
	}
	return mount + "/.lever"
}

// acceptanceHarness drives the acceptance checks against the live jail. jr execs
// `lever-agent` inside the jail machine.
type acceptanceHarness struct {
	app       *config.App
	jr        *jail.Runner
	hostAlias string // host alias reachable from the jail (host.orb.internal); the broker listens behind it
	bootDir   string // in-jail dir containing bootstrap.json (broker URL + CA)
	managerID string // in-jail dir holding the manager's mTLS identity (the delegator)
	workerID  string // in-jail dir holding the worker's mTLS identity (the executor)
}

// vmIDDir returns a distinct, non-empty, VM-writable identity directory per role.
// The broker-only VM has no agent containers (so no /home/manager|worker users);
// these dirs live under /tmp, writable by the run user the JailRunner execs as.
// Pure (no I/O) so it is unit-testable without a live jail.
func vmIDDir(role string) string {
	return "/tmp/lever-acceptance/" + role + "/.lever-id"
}

// hostAgentBinPath resolves the HOST path of the linux/arm64 lever-agent binary
// to copy into the VM: $LEVER_AGENT_BIN if set, else $LEVER_INSTANCE/vendor/bin/
// lever-agent (the `make lever-agent-linux` output; $LEVER_INSTANCE defaults to
// $HOME/lever-instance). It errors with a build hint if the resolved path does not
// exist, so the gate FAILS CLOSED rather than silently skipping the install.
func hostAgentBinPath() (string, error) {
	p := os.Getenv("LEVER_AGENT_BIN")
	if p == "" {
		inst := os.Getenv("LEVER_INSTANCE")
		if inst == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home for lever-agent binary: %w", err)
			}
			inst = filepath.Join(home, "lever-instance")
		}
		p = filepath.Join(inst, "vendor", "bin", "lever-agent")
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("lever-agent binary not found at %s (build it: `make lever-agent-linux`, or set LEVER_AGENT_BIN): %w", p, err)
	}
	return p, nil
}

// installLeverAgent copies the host-built linux/arm64 lever-agent into the VM at
// /usr/local/bin/lever-agent (mode 0755) AS ROOT, so the existing
// jr.Run(ctx, nil, "lever-agent", ...) finds it on PATH. Isolated OrbStack
// machines do NOT mount the Mac filesystem, so we pipe the binary in as stdin to
// `orb -u root -m <machine> sh -c 'cat > … && chmod …'`. We use os/exec directly
// because the JailRunner has no stdin channel.
func installLeverAgent(ctx context.Context, machine string) error {
	src, err := hostAgentBinPath()
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open lever-agent binary %s: %w", src, err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "orb", "-u", "root", "-m", machine, "sh", "-c",
		"cat > /usr/local/bin/lever-agent && chmod 0755 /usr/local/bin/lever-agent")
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install lever-agent into jail %s: %w: %s", machine, err, string(out))
	}
	return nil
}

// setup makes the VM gate runnable: install lever-agent, create the VM-writable
// identity-dir parents, enrol the manager from the deposited bootstrap, then
// provision a worker ticket (as manager) and enrol the worker. Every step is
// fail-closed — a setup error aborts the gate (never a vacuous check pass).
func (h *acceptanceHarness) setup(ctx context.Context, machine string) error {
	// 1) Install lever-agent into the VM (copied in as root; isolated machines
	//    have no Mac-fs mount, so a shared-mount exec is impossible).
	if err := installLeverAgent(ctx, machine); err != nil {
		return err
	}
	// 2) Ensure the per-role identity-dir parents exist and are run-user-writable.
	for _, dir := range []string{h.managerID, h.workerID} {
		if out, err := h.jr.Run(ctx, nil, "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("mkdir -p %s in jail: %w: %s", dir, err, out.Stdout+out.Stderr)
		}
	}
	// 3) Enrol the manager from the deposited bootstrap.json.
	bootstrap := h.bootDir + "/bootstrap.json"
	if res, err := h.jr.Run(ctx, nil, "lever-agent", "boot", "-enrol-only", "-id-dir", h.managerID, "-bootstrap", bootstrap); err != nil {
		return fmt.Errorf("enrol manager (lever-agent boot): %w: %s", err, res.Stdout+res.Stderr)
	}
	// 4) Provision a worker ticket AS the manager (manager-CN-gated /provision),
	//    writing the worker bootstrap to a VM-writable path, then enrol the worker.
	// TODO(hardening): this fixed /tmp path (and the vmIDDir parents) would collide
	// across concurrent `lever acceptance` runs; fine for the single-run merge gate.
	wbs := "/tmp/lever-acceptance/worker-bootstrap.json"
	if res, err := h.jr.Run(ctx, nil, "lever-agent", "provision", "-grove", "worker", "-out", wbs, "-id-dir", h.managerID, "-bootstrap", bootstrap); err != nil {
		return fmt.Errorf("provision worker (lever-agent provision): %w: %s", err, res.Stdout+res.Stderr)
	}
	if res, err := h.jr.Run(ctx, nil, "lever-agent", "boot", "-enrol-only", "-id-dir", h.workerID, "-bootstrap", wbs); err != nil {
		return fmt.Errorf("enrol worker (lever-agent boot): %w: %s", err, res.Stdout+res.Stderr)
	}
	return nil
}

// agentAs runs `lever-agent <verb> <args...>` inside the jail using the given
// identity dir, returning combined stdout/stderr + error. The bootstrap path
// points lever-agent at the broker (URL + CA).
func (h *acceptanceHarness) agentAs(ctx context.Context, idDir, verb string, args ...string) (string, error) {
	bootstrap := h.bootDir + "/bootstrap.json"
	full := append([]string{verb, "-id-dir", idDir, "-bootstrap", bootstrap}, args...)
	res, err := h.jr.Run(ctx, nil, "lever-agent", full...)
	return res.Stdout + res.Stderr, err
}

// agent runs as the worker (the default subject of the executor-side checks).
func (h *acceptanceHarness) agent(ctx context.Context, verb string, args ...string) (string, error) {
	return h.agentAs(ctx, h.workerID, verb, args...)
}

// run dispatches one acceptance check. Each closure ACTUALLY attempts its operation;
// pass/fail is derived from the live outcome (allowed vs denied), never hard-coded.
func (h *acceptanceHarness) run(ctx context.Context, name string) (bool, error) {
	switch name {
	case "delegated-read":
		return h.checkDelegatedRead(ctx)
	case "no-table-c":
		return h.checkNoTableC(ctx)
	case "no-drop-filter":
		return h.checkNoDropFilter(ctx)
	case "no-self-path":
		return h.checkNoSelfPath(ctx)
	case "egress-refused":
		return h.checkEgressRefused(ctx)
	case "revocation":
		return h.checkRevocation(ctx)
	default:
		return false, fmt.Errorf("unknown check %q", name)
	}
}

// workerToken obtains the worker's delegated db.read token. The worker has an
// EMPTY obtain (no ambient authority), so it cannot self-mint — the MANAGER
// mints the token via its `delegate db.read → worker` grant (bound_to=worker),
// and the worker then attenuates the narrowing filter so it only sees its rows.
func (h *acceptanceHarness) workerToken(ctx context.Context, withFilter bool) (string, error) {
	// Manager delegates db.read to the worker (the manager holds the delegate grant).
	out, err := h.agentAs(ctx, h.managerID, "delegate", "-tool", "db", "-op", "read", "-to", "worker", "table=A")
	if err != nil {
		return "", fmt.Errorf("manager delegate db.read → worker: %w: %s", err, out)
	}
	tok := strings.TrimSpace(lastLine(out))
	if !withFilter {
		return tok, nil
	}
	// Worker attenuates: append the narrowing filter caveat (offline, no broker).
	att, err := h.agent(ctx, "attenuate", "-token", tok, "filter=owner=worker")
	if err != nil {
		return "", fmt.Errorf("worker attenuate filter: %w: %s", err, att)
	}
	return strings.TrimSpace(lastLine(att)), nil
}

// checkDelegatedRead — PASS = worker's delegated+attenuated token reads its
// filtered rows (the call is ALLOWED and returns the filtered result).
func (h *acceptanceHarness) checkDelegatedRead(ctx context.Context) (bool, error) {
	tok, err := h.workerToken(ctx, true)
	if err != nil {
		return false, err
	}
	out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A", "filter=owner=worker")
	if err != nil {
		return false, fmt.Errorf("delegated read denied (should be allowed): %w: %s", err, out)
	}
	return true, nil
}

// checkNoTableC — PASS = worker CANNOT read a table/scope outside the delegated
// envelope (table C). The call MUST be denied.
func (h *acceptanceHarness) checkNoTableC(ctx context.Context) (bool, error) {
	tok, err := h.workerToken(ctx, true)
	if err != nil {
		return false, err
	}
	out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=C", "filter=owner=worker")
	if err != nil {
		return true, nil // denied — correct
	}
	return false, fmt.Errorf("table C read was ALLOWED (must be denied): %s", out)
}

// checkNoDropFilter — PASS = worker CANNOT widen by dropping the attenuated
// narrowing filter. Calling with the filter omitted MUST be denied.
func (h *acceptanceHarness) checkNoDropFilter(ctx context.Context) (bool, error) {
	tok, err := h.workerToken(ctx, true) // token carries the attenuated filter caveat
	if err != nil {
		return false, err
	}
	// Omit the filter argument: the attenuated caveat is not satisfied.
	out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A")
	if err != nil {
		return true, nil // denied — the dropped filter is caught
	}
	return false, fmt.Errorf("read with the narrowing filter dropped was ALLOWED (must be denied): %s", out)
}

// checkNoSelfPath — PASS = worker CANNOT self-mint a capability it was never
// delegated (no ambient authority). A /request for an un-granted cap MUST be
// refused by the broker.
func (h *acceptanceHarness) checkNoSelfPath(ctx context.Context) (bool, error) {
	// The worker self-mints db.read bound to ITSELF. It has an empty obtain, so
	// MayObtain("worker","worker","db","read") is false — the broker's /request
	// MUST refuse it. (The worker may only exercise a token the MANAGER delegated;
	// it has no ambient authority of its own.)
	//
	// Assumption: if lever-agent is absent entirely, workerToken() (called by other
	// checks before this one) already fails hard, so we don't need to guard that
	// case here separately.
	out, err := h.agent(ctx, "request", "-tool", "db", "-op", "read", "table=A")
	if err != nil {
		// Non-zero exit = broker refused (or lever-agent signalled denial). This
		// is the expected outcome — the broker's /request MUST deny a worker with
		// no ambient authority. PASS.
		return true, nil
	}
	// Exit 0 with empty output is AMBIGUOUS: we cannot confirm denial occurred.
	// Treat as FAIL-CLOSED rather than recording a vacuous PASS.
	if strings.TrimSpace(out) == "" {
		return false, fmt.Errorf("no-self-path: ambiguous result — request exited 0 with empty output (expected broker denial / non-zero exit)")
	}
	// Exit 0 with any output means the broker ALLOWED the request — escalation.
	return false, fmt.Errorf("worker self-minted an un-granted cap (must be refused): %s", out)
}

// classifyCurlResult maps an exec.Result from a curl invocation to a tri-state:
//
//   - ("blocked", nil)     -- genuine policy block (curl exit 7 or 28)
//   - ("allowed", nil)     -- curl connected successfully (exit 0)
//   - ("uncertain", error) -- curl not found (exit 127) or any other unexpected
//     exit -- FAIL-CLOSED
//
// res.Code is reliable here: jail.Runner wraps exec.RealRunner via `orb`, which
// passes the inner command's exit code back through *exec.ExitError.ExitCode()
// even on the non-zero path (see internal/exec/runner.go). Exit 0 means curl
// succeeded (egress open); exit 7 is CURLE_COULDNT_CONNECT (ECONNREFUSED /
// network-unreachable); exit 28 is CURLE_OPERATION_TIMEDOUT (packet dropped);
// exit 127 is command-not-found from the shell.
func classifyCurlResult(res leverexec.Result, err error) (string, error) {
	if err == nil {
		return "allowed", nil
	}
	switch res.Code {
	case 7, 28:
		// Genuine egress-block signatures: connection rejected or max-time
		// budget expired because the packet was dropped by the policy.
		return "blocked", nil
	case 127:
		return "uncertain", fmt.Errorf("egress-refused: curl not found in the jail image (exit 127) -- check is UNCERTAIN; image must include curl: %s", res.Stderr)
	default:
		// Any other curl exit (DNS failure, SSL error, etc.) is ambiguous:
		// we cannot tell whether egress was open or blocked. FAIL-CLOSED.
		combined := res.Stdout + res.Stderr
		// Guard against shells that emit 126 or the orb wrapper absorbing 127.
		if strings.Contains(combined, "not found") || strings.Contains(combined, "No such file") {
			return "uncertain", fmt.Errorf("egress-refused: curl not found in the jail image -- check is UNCERTAIN; image must include curl: %s", combined)
		}
		return "uncertain", fmt.Errorf("egress-refused: curl exited %d with unexpected output -- check is UNCERTAIN (FAIL-CLOSED): %s", res.Code, combined)
	}
}

// classifyEgressProbe maps a curl probe of a host:port to "reachable"/"blocked"/
// "uncertain". Unlike classifyCurlResult, a TLS-handshake-level failure counts as
// REACHABLE: the egress allowlist works at the TCP layer, so if the packet got
// far enough to start a TLS handshake (curl exit 35 SSL connect error / 60 cert
// verify failure), the TCP connection SUCCEEDED — connecting is the point. Exit 0
// is reachable; exit 7 (refused) / 28 (timeout/dropped) is blocked; exit 127
// (curl absent) or anything else is uncertain (FAIL-CLOSED).
func classifyEgressProbe(res leverexec.Result, err error) (string, error) {
	if err == nil {
		return "reachable", nil
	}
	switch res.Code {
	case 35, 60:
		// TLS-layer failure: the TCP connection was established, so the port is
		// reachable through the allowlist (the contrast we want to prove).
		return "reachable", nil
	case 7, 28:
		// CURLE_COULDNT_CONNECT (refused / net-unreachable) or
		// CURLE_OPERATION_TIMEDOUT (packet dropped by the policy) = blocked.
		return "blocked", nil
	case 127:
		return "uncertain", fmt.Errorf("egress-refused: curl not found in the jail (exit 127): %s", res.Stderr)
	default:
		combined := res.Stdout + res.Stderr
		if strings.Contains(combined, "not found") || strings.Contains(combined, "No such file") {
			return "uncertain", fmt.Errorf("egress-refused: curl not found in the jail: %s", combined)
		}
		return "uncertain", fmt.Errorf("egress-refused: curl exited %d with unexpected output (FAIL-CLOSED): %s", res.Code, combined)
	}
}

// egressVerdict is the PURE decision for the egress check: PASS iff the
// allowlisted broker jail port is REACHABLE and the non-allowlisted admin port is
// BLOCKED — a non-vacuous contrast that directly proves the allowlist. Any other
// combination FAILS (and an "uncertain"/unreachable jail port or a reachable
// admin port FAILS CLOSED). Unit-testable without a live jail.
func egressVerdict(jailState, adminState string) (bool, error) {
	if jailState != "reachable" {
		return false, fmt.Errorf("egress-refused: broker jail port not reachable (got %q) — allowlist or broker down; FAIL-CLOSED", jailState)
	}
	if adminState != "blocked" {
		return false, fmt.Errorf("egress-refused: broker ADMIN port was %q (must be blocked) — allowlist not containing the jail", adminState)
	}
	return true, nil
}

// probeReachable curls host:port from inside the jail and classifies the result
// as reachable/blocked/uncertain (TLS-handshake errors count as reachable).
func (h *acceptanceHarness) probeReachable(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("https://%s:%d/", h.hostAlias, port)
	res, err := h.jr.Run(ctx, nil, "curl", "-sS", "--connect-timeout", "4", "--max-time", "5", url)
	return classifyEgressProbe(res, err)
}

// checkEgressRefused — PASS = the jail CAN reach the allowlisted broker JAIL port
// AND CANNOT reach the non-allowlisted broker ADMIN port. Both listen on the host
// behind host.orb.internal, so the contrast is non-vacuous and directly proves
// the egress allowlist. FAIL-CLOSED on any ambiguous result. (The public internet
// is intentionally left OPEN by the allowlist, so curling example.com would NOT
// test containment — the broker port contrast does.)
func (h *acceptanceHarness) checkEgressRefused(ctx context.Context) (bool, error) {
	jailState, jerr := h.probeReachable(ctx, h.app.EffectiveJailPort())
	if jailState == "uncertain" {
		return false, jerr // FAIL-CLOSED: cannot classify the jail-port probe
	}
	adminState, aerr := h.probeReachable(ctx, h.app.EffectiveAdminPort())
	if adminState == "uncertain" {
		return false, aerr // FAIL-CLOSED: cannot classify the admin-port probe
	}
	return egressVerdict(jailState, adminState)
}

// checkRevocation — PASS = after /bump-epoch the worker's previously-working
// capability call is denied (403). We mint+exercise a token, bump the epoch,
// then re-exercise: the second call MUST fail.
func (h *acceptanceHarness) checkRevocation(ctx context.Context) (bool, error) {
	tok, err := h.workerToken(ctx, true)
	if err != nil {
		return false, err
	}
	// Prove it works first.
	if out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A", "filter=owner=worker"); err != nil {
		return false, fmt.Errorf("pre-revocation call should succeed: %w: %s", err, out)
	}
	// Revoke by raising the epoch floor (admin endpoint on the host loopback).
	if err := adminPost(ctx, h.app, "/bump-epoch", nil); err != nil {
		return false, fmt.Errorf("bump-epoch: %w", err)
	}
	// The same token must now be denied.
	out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A", "filter=owner=worker")
	if err != nil {
		return true, nil // denied post-revocation — correct
	}
	return false, fmt.Errorf("token still accepted after bump-epoch (must be 403): %s", out)
}

// lastLine returns the last non-empty line of s (the CLI prints the token/result
// on its own line; preceding lines may be diagnostics).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}
