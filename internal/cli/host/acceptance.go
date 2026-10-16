package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/wire"
)

// acceptanceCheckNames is the fixed order of the six checks; formatNote
// iterates it, not map order, so the note is deterministic.
func acceptanceCheckNames() []string {
	return []string{
		"delegated-read", // worker CAN read its delegated, filtered rows
		"no-table-c",     // worker CANNOT read a scope outside the envelope
		"no-drop-filter", // worker CANNOT widen by dropping the delegated filter caveat
		"no-self-path",   // worker CANNOT self-mint an un-granted capability
		"egress-refused", // jail reaches the allowlisted broker port but NOT the non-allowlisted admin port, and the metadata REJECT is exactly its /32
		"revocation",     // after revoke/bump-epoch the next call is denied
	}
}

// overallPass is true iff every named check passed; a check that never ran
// fails the gate.
func overallPass(results map[string]bool) bool {
	for _, name := range acceptanceCheckNames() {
		if !results[name] {
			return false
		}
	}
	return true
}

// formatNote renders a dated markdown acceptance note with an overall verdict
// that fails if any check fails or is missing.
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
	var progressMode string
	var lockWait time.Duration
	c := &cobra.Command{
		Use:   "acceptance [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring up a real jail and drive the six acceptance capability checks (live gate)",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			// It starts the broker and mints the manager's ticket, as apply does.
			unlock, err := lockLifecycle(cmd.Context(), app, stateFor(path), "lever acceptance", lockWait, lockNotice(cmd))
			if err != nil {
				return err
			}
			defer unlock()
			// The bring-up is listed and timed like apply's: the e2e reads the phase times
			// from its record.
			run, ctx, err := startBringup(cmd, app, stateFor(path), "acceptance", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			return runAcceptance(ctx, cmd, app, path, bf)
		},
	}
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	addLockWaitFlag(c, &lockWait, 0)
	return c
}

// runAcceptance is the live acceptance gate. It brings a real jail up (manager,
// executor worker `worker`, the `db` tool; manager delegate db.read to worker;
// worker empty obtain), runs the six checks inside it, writes a dated note to
// docs/acceptance/, and fails if any check fails.
//
// The check closures are validated live, not in CI. Each actually attempts its
// operation through the in-jail `lever-agent`; a check that cannot be driven
// records a failure, never a vacuous pass.
func runAcceptance(ctx context.Context, cmd *cobra.Command, app *config.App, configPath string, bf BackendFactory) error {
	// Bring the jail up broker-only: the gate drives lever-agent directly and never
	// invokes scion, so the scion, container and registration steps are omitted
	// (init-machine would need a scion binary the fresh machine lacks). The
	// bootstrap step still deposits <mount>/.lever/bootstrap.json.
	rep := progress.From(ctx)
	rep.Group("jail", 0)
	w, err := buildApplyDeps(ctx, app, configPath, bf, applyOpts{Cmd: cmd, Rep: rep})
	if err != nil {
		return fmt.Errorf("acceptance: bring-up deps: %w", err)
	}
	b := w.b
	if err := apply.Run(ctx, app, w.deps, apply.PlanOpts{BrokerOnly: true}); err != nil {
		return fmt.Errorf("acceptance: apply: %w", err)
	}

	// lever-agent runs inside the jail machine. The manager mints the worker's
	// delegated token, exercised from the worker's identity directory. Identity
	// dirs are VM-writable (vmIDDir): the broker-only VM has no agent users.
	jr := b.JailRunner()
	h := &acceptanceHarness{
		app:       app,
		jr:        jr,
		hostAlias: b.HostToolAlias(),
		bootDir:   bootstrapDirInJail(app, b.MountDest()),
		managerID: vmIDDir("manager"),
		workerID:  vmIDDir("worker"),
		adminURL:  fmt.Sprintf("http://127.0.0.1:%d", app.EffectiveAdminPort()),
		admin:     &http.Client{Timeout: brokerAdminTimeout},
	}

	// Setup fails closed: any error aborts the gate, never a vacuous pass.
	rep.Group("acceptance", 0)
	setup := rep.Start("enrol test agents")
	err = h.setup(ctx, b)
	setup.Done(err)
	if err != nil {
		return fmt.Errorf("acceptance: setup: %w", err)
	}
	// Wait until every supervised tool listens: a check that reaches the gateway
	// before its tool binds gets a 502 that says nothing about the capability
	// under test.
	tools := rep.Start("wait for tools to listen")
	err = waitToolsListening(ctx, app, defaultToolDial, toolListenWait)
	tools.Done(err)
	if err != nil {
		return fmt.Errorf("acceptance: %w", err)
	}
	checks := rep.Start("capability checks")

	results := map[string]bool{}
	for _, name := range acceptanceCheckNames() {
		ok, derr := h.run(ctx, name)
		results[name] = ok
		if derr != nil {
			acceptanceLine(cmd, rep, fmt.Sprintf("check %-14s error: %v", name, derr))
		}
		status := "FAIL"
		if ok {
			status = "PASS"
		}
		acceptanceLine(cmd, rep, fmt.Sprintf("check %-14s %s", name, status))
	}
	if overallPass(results) {
		checks.Done(nil)
	} else {
		checks.Done(errors.New("a capability check failed"))
	}

	date := time.Now().Format("2006-01-02")
	note := formatNote(results, date)
	// The note lives beside the instance root, not inside the agent-writable tree.
	noteDir := filepath.Join(filepath.Dir(configPath), "docs", "acceptance")
	if err := os.MkdirAll(noteDir, 0o755); err != nil {
		return fmt.Errorf("acceptance: mkdir note dir: %w", err)
	}
	notePath := filepath.Join(noteDir, date+"-acceptance.md")
	if err := os.WriteFile(notePath, []byte(note), 0o644); err != nil {
		return fmt.Errorf("acceptance: write note: %w", err)
	}
	cmd.Printf("acceptance note: %s\n", notePath)

	if !overallPass(results) {
		return fmt.Errorf("acceptance: acceptance gate FAILED — see %s", notePath)
	}
	cmd.Println("acceptance PASSED")
	return nil
}

func acceptanceLine(cmd *cobra.Command, rep *progress.Reporter, line string) {
	if rep != nil {
		rep.Log(line)
		return
	}
	cmd.Println(line)
}

// bootstrapDirInJail is the in-jail directory of the manager's bootstrap.json
// (<tree>/.lever, mounted at <mount>/.lever), from which lever-agent reads the
// broker URL and CA.
func bootstrapDirInJail(app *config.App, mount string) string {
	if mount == "" {
		return filepath.Join(app.Tree, ".lever")
	}
	return mount + "/.lever"
}

type acceptanceHarness struct {
	app       *config.App
	jr        proc.Runner
	hostAlias string // host alias reachable from the jail (host.orb.internal); the broker listens behind it
	bootDir   string // in-jail dir containing bootstrap.json (broker URL + CA)
	managerID string // in-jail dir holding the manager's mTLS identity (the delegator)
	workerID  string // in-jail dir holding the worker's mTLS identity (the executor)
	// The worker's ticket is minted host-side on the admin API (/worker-ticket)
	// and staged into the guest by the broker, as a real dispatch does.
	adminURL string
	admin    *http.Client
}

// vmIDDir returns a distinct VM-writable identity directory per role, under
// /tmp: the broker-only VM has no agent users.
func vmIDDir(role string) string {
	return "/tmp/lever-acceptance/" + role + "/.lever-id"
}

// hostAgentBinPath resolves the host's linux lever-agent binary:
// $LEVER_AGENT_BIN, else $LEVER_INSTANCE/vendor/bin/lever-agent ($LEVER_INSTANCE
// defaults to $HOME/lever-instance). A missing file is an error with a build
// hint, so the gate fails closed.
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

// installLeverAgent copies lever-agent into the guest at
// /usr/local/bin/lever-agent as root, streamed over the backend's root
// transport: isolated guests do not mount the host filesystem.
func installLeverAgent(ctx context.Context, b backend.Backend) error {
	src, err := hostAgentBinPath()
	if err != nil {
		return err
	}
	if err := b.InstallGuestBinary(ctx, src, "/usr/local/bin/lever-agent"); err != nil {
		return fmt.Errorf("install lever-agent into jail: %w", err)
	}
	return nil
}

// setup installs lever-agent, creates the identity-dir parents, enrols the
// manager from the deposited bootstrap, then mints a worker ticket host-side
// and enrols the worker from where the broker staged it. Every step fails
// closed.
func (h *acceptanceHarness) setup(ctx context.Context, b backend.Backend) error {
	if err := installLeverAgent(ctx, b); err != nil {
		return err
	}
	for _, dir := range []string{h.managerID, h.workerID} {
		if out, err := h.jr.Run(ctx, nil, "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("mkdir -p %s in jail: %w: %s", dir, err, out.Stdout+out.Stderr)
		}
	}
	bootstrap := h.bootDir + "/bootstrap.json"
	if res, err := h.jr.Run(ctx, nil, "lever-agent", "boot", "-enrol-only", "-id-dir", h.managerID, "-bootstrap", bootstrap); err != nil {
		return fmt.Errorf("enrol manager (lever-agent boot): %w: %s", err, res.Stdout+res.Stderr)
	}
	// The worker's ticket is minted host-side through the admin API and staged by
	// the broker in the guest runtime dir, the channel a dispatch uses; nothing an
	// agent can reach mints a worker ticket.
	// TODO(hardening): the vmIDDir parents would collide across concurrent
	// `lever acceptance` runs; fine for the single-run merge gate.
	var wt wire.WorkerTicketResponse
	if err := httpjson.Post(ctx, h.admin, h.adminURL+wire.PathWorkerTicket, wire.WorkerTicketRequest{Worker: "worker"}, &wt); err != nil {
		return fmt.Errorf("mint worker ticket (broker %s): %w", wire.PathWorkerTicket, err)
	}
	if res, err := h.jr.Run(ctx, nil, "lever-agent", "boot", "-enrol-only", "-id-dir", h.workerID, "-bootstrap", wt.Path); err != nil {
		return fmt.Errorf("enrol worker (lever-agent boot): %w: %s", err, res.Stdout+res.Stderr)
	}
	return nil
}

// agentAs runs `lever-agent <verb> <args...>` in the jail with the given
// identity dir and returns combined output.
func (h *acceptanceHarness) agentAs(ctx context.Context, idDir, verb string, args ...string) (string, error) {
	bootstrap := h.bootDir + "/bootstrap.json"
	full := append([]string{verb, "-id-dir", idDir, "-bootstrap", bootstrap}, args...)
	res, err := h.jr.Run(ctx, nil, "lever-agent", full...)
	return res.Stdout + res.Stderr, err
}

func (h *acceptanceHarness) agent(ctx context.Context, verb string, args ...string) (string, error) {
	return h.agentAs(ctx, h.workerID, verb, args...)
}

// run dispatches one check. Pass or fail comes from the live outcome, never
// hard-coded.
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
// empty obtain, so the manager mints it (bound_to=worker) with the narrowing
// caveats baked in at mint: delegation is online-only, so the worker can only
// use a token that already carries caveats it cannot drop.
func (h *acceptanceHarness) workerToken(ctx context.Context, withFilter bool) (string, error) {
	extra := []string{"-tool", "db", "-op", "read", "-to", "worker", "table=A"}
	if withFilter {
		extra = append(extra, "filter=owner=worker")
	}
	out, err := h.agentAs(ctx, h.managerID, "delegate", extra...)
	if err != nil {
		return "", fmt.Errorf("manager delegate db.read → worker: %w: %s", err, out)
	}
	return strings.TrimSpace(lastLine(out)), nil
}

// checkDelegatedRead passes when the worker's delegated, filtered token is
// allowed to read its rows.
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

// checkNoTableC passes when a read outside the delegated envelope is denied.
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

// checkNoDropFilter passes when a call omitting the delegated filter caveat
// is denied.
func (h *acceptanceHarness) checkNoDropFilter(ctx context.Context) (bool, error) {
	tok, err := h.workerToken(ctx, true) // token carries the delegated filter caveat
	if err != nil {
		return false, err
	}
	out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A")
	if err != nil {
		return true, nil // denied — the dropped filter is caught
	}
	return false, fmt.Errorf("read with the narrowing filter dropped was ALLOWED (must be denied): %s", out)
}

// checkNoSelfPath passes when the broker refuses the worker's /request for a
// capability it was never delegated.
func (h *acceptanceHarness) checkNoSelfPath(ctx context.Context) (bool, error) {
	// The worker has an empty obtain, so the broker's /request must refuse a
	// self-mint. A missing lever-agent already fails the earlier checks.
	out, err := h.agent(ctx, "request", "-tool", "db", "-op", "read", "table=A")
	if err != nil {
		return true, nil
	}
	// Exit 0 with empty output cannot confirm a denial: fail closed.
	if strings.TrimSpace(out) == "" {
		return false, fmt.Errorf("no-self-path: ambiguous result — request exited 0 with empty output (expected broker denial / non-zero exit)")
	}
	// Exit 0 with output means the broker allowed it: an escalation.
	return false, fmt.Errorf("worker self-minted an un-granted cap (must be refused): %s", out)
}

type probeState string

const (
	stateReachable probeState = "reachable" // TCP connection succeeded (incl. TLS-layer failures)
	stateBlocked   probeState = "blocked"   // refused or dropped by the egress policy
	stateUncertain probeState = "uncertain" // cannot classify — FAIL-CLOSED
)

// classifyEgressProbe maps a curl probe to reachable, blocked or uncertain.
// The allowlist works at the TCP layer, so a TLS-layer failure (curl 35, 60)
// means the connection succeeded: reachable. 0 is reachable; 7 (refused) and
// 28 (dropped) are blocked; anything else is uncertain (fail closed).
func classifyEgressProbe(res proc.Result, err error) (probeState, error) {
	if err == nil {
		return stateReachable, nil
	}
	switch res.Code {
	case 35, 60:
		return stateReachable, nil
	case 7, 28:
		return stateBlocked, nil
	case 127:
		return stateUncertain, fmt.Errorf("egress-refused: curl not found in the jail (exit 127): %s", res.Stderr)
	default:
		combined := res.Stdout + res.Stderr
		if strings.Contains(combined, "not found") || strings.Contains(combined, "No such file") {
			return stateUncertain, fmt.Errorf("egress-refused: curl not found in the jail: %s", combined)
		}
		return stateUncertain, fmt.Errorf("egress-refused: curl exited %d with unexpected output (FAIL-CLOSED): %s", res.Code, combined)
	}
}

// egressVerdict passes iff the allowlisted broker jail port is reachable and
// the non-allowlisted admin port is blocked: a non-vacuous contrast that
// proves the allowlist. Anything else fails closed.
func egressVerdict(jailState, adminState probeState) (bool, error) {
	if jailState != stateReachable {
		return false, fmt.Errorf("egress-refused: broker jail port not reachable (got %q) — allowlist or broker down; FAIL-CLOSED", jailState)
	}
	if adminState != stateBlocked {
		return false, fmt.Errorf("egress-refused: broker ADMIN port was %q (must be blocked) — allowlist not containing the jail", adminState)
	}
	return true, nil
}

func (h *acceptanceHarness) probeReachable(ctx context.Context, port int) (probeState, error) {
	url := fmt.Sprintf("https://%s:%d/", h.hostAlias, port)
	res, err := h.jr.Run(ctx, nil, "curl", "-sS", "--connect-timeout", "4", "--max-time", "5", url)
	return classifyEgressProbe(res, err)
}

// checkEgressRefused passes when the jail reaches the allowlisted broker jail
// port but not the admin port; both listen behind the host alias, so the
// contrast proves the allowlist (the public internet is open by design, so a
// public URL would prove nothing). It then checks the chain's one REJECT
// (metadataVerdict): the metadata address is refused at once and a
// neighbouring link-local address is still dropped.
func (h *acceptanceHarness) checkEgressRefused(ctx context.Context) (bool, error) {
	jailState, jerr := h.probeReachable(ctx, h.app.EffectiveJailPort())
	if jailState == stateUncertain {
		return false, jerr // FAIL-CLOSED: cannot classify the jail-port probe
	}
	adminState, aerr := h.probeReachable(ctx, h.app.EffectiveAdminPort())
	if adminState == stateUncertain {
		return false, aerr // FAIL-CLOSED: cannot classify the admin-port probe
	}
	if ok, err := egressVerdict(jailState, adminState); !ok {
		return false, err
	}
	// The REJECT must be exactly the metadata /32 and open nothing.
	meta, merr := h.probeLinkLocal(ctx, egress.MetadataV4)
	if meta == linkUncertain {
		return false, merr
	}
	other, oerr := h.probeLinkLocal(ctx, linkLocalControl)
	if other == linkUncertain {
		return false, oerr
	}
	return metadataVerdict(meta, other)
}

// linkLocalControl is a link-local address next to egress.MetadataV4 that the
// chain must drop, not reject: the control for the REJECT.
const linkLocalControl = "169.254.169.253"

type linkState string

const (
	linkOpen      linkState = "open"      // the dial reached something (connected, or a host answered with a reset)
	linkRejected  linkState = "rejected"  // EHOSTUNREACH: the chain's icmp-host-prohibited REJECT
	linkDropped   linkState = "dropped"   // no answer before the timeout: a DROP
	linkUncertain linkState = "uncertain" // cannot classify — FAIL-CLOSED
)

// linkLocalTimeout bounds one link-local dial, in seconds: a DROP costs this
// long, a REJECT returns at once.
const linkLocalTimeout = "3"

// probeLinkLocal dials ip:80 with bash's /dev/tcp, which unlike curl reports
// the connect errno, so a REJECT and a reset can be told apart. ip is one of
// two constants, never input.
func (h *acceptanceHarness) probeLinkLocal(ctx context.Context, ip string) (linkState, error) {
	res, err := h.jr.Run(ctx, nil, "timeout", linkLocalTimeout, "bash", "-c", "exec 3<>/dev/tcp/"+ip+"/80")
	return classifyLinkLocalProbe(ip, res, err)
}

// classifyLinkLocalProbe: exit 0 and "Connection refused" (a reset reached a
// host) are open; "No route to host" is rejected; timeout's exit 124 is
// dropped; anything else is uncertain.
func classifyLinkLocalProbe(ip string, res proc.Result, err error) (linkState, error) {
	if err == nil {
		return linkOpen, nil
	}
	out := res.Stdout + res.Stderr
	switch {
	case res.Code == 124:
		return linkDropped, nil
	case strings.Contains(out, "No route to host"):
		return linkRejected, nil
	case strings.Contains(out, "Connection refused"):
		return linkOpen, nil
	default:
		return linkUncertain, fmt.Errorf("egress-refused: link-local probe of %s exited %d with unexpected output (FAIL-CLOSED): %s", ip, res.Code, out)
	}
}

// metadataVerdict passes iff the metadata address is rejected and the control
// is dropped. A dropped metadata address is contained but means the REJECT is
// not in force or its ICMP answer is lost (no lo ACCEPT); a rejected control
// means the REJECT is wider than its /32; an open address is a hole.
func metadataVerdict(meta, control linkState) (bool, error) {
	if meta != linkRejected {
		return false, fmt.Errorf("egress-refused: metadata address %s was %q, want %q (the chain's icmp-host-prohibited REJECT and the lo ACCEPT for its answer; a closed chain from before them needs lever down and up)", egress.MetadataV4, meta, linkRejected)
	}
	if control != linkDropped {
		return false, fmt.Errorf("egress-refused: link-local control %s was %q, want %q (only the metadata /32 may be rejected)", linkLocalControl, control, linkDropped)
	}
	return true, nil
}

// checkRevocation passes when a working token is denied after /bump-epoch.
func (h *acceptanceHarness) checkRevocation(ctx context.Context) (bool, error) {
	tok, err := h.workerToken(ctx, true)
	if err != nil {
		return false, err
	}
	if out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A", "filter=owner=worker"); err != nil {
		return false, fmt.Errorf("pre-revocation call should succeed: %w: %s", err, out)
	}
	if err := adminPost(ctx, h.app, "/bump-epoch", nil); err != nil {
		return false, fmt.Errorf("bump-epoch: %w", err)
	}
	out, err := h.agent(ctx, "call", "-tool", "db", "-op", "read", "-token", tok, "table=A", "filter=owner=worker")
	if err != nil {
		return true, nil // denied post-revocation — correct
	}
	return false, fmt.Errorf("token still accepted after bump-epoch (must be 403): %s", out)
}

const toolListenWait = 30 * time.Second

func defaultToolDial(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// waitToolsListening polls each supervised tool's backend until it accepts a
// connection, for at most wait in all. A tool that does not listen is an error
// naming the tool, its address and its log, so the gate fails on the real
// cause instead of a 502 in a check.
func waitToolsListening(ctx context.Context, app *config.App, dial func(context.Context, string) error, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	// A tool whose program lies under manager.read_only waits for the manager's
	// mounts, which a broker-only run never starts.
	held, err := app.ToolsOnReadOnly()
	if err != nil {
		return fmt.Errorf("which tools manager.read_only holds back: %w", err)
	}
	for _, t := range app.Broker.Tools {
		if t.External || t.Backend == "" || len(held[t.Name]) > 0 {
			continue
		}
		addr := backendHostPort(t.Backend)
		for {
			err := dial(ctx, addr)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("tool %q is not listening on %s after %s (%v); see its log, tool-logs/%s.log in the state directory", t.Name, addr, wait, err, t.Name)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}
