package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/remoteproxy"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
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

// doctorProbes is every host-side observation doctor makes that reaches
// outside the process: TCP dials, HTTP requests to the remote proxy, and
// subprocesses. Checks take the struct rather than a package variable so a
// test builds its own value and never races another test's override.
// productionProbes builds the real one.
type doctorProbes struct {
	// dial reports whether something listens on a TCP address.
	dial dialFunc
	// goVersion runs `go version` on the host PATH.
	goVersion func() (string, error)
	// nodeToolchain validates node+npm for the scion web-asset build and
	// returns the node version.
	nodeToolchain func() (string, error)
	// claudeVersion reads the baked Claude Code version label of an image.
	claudeVersion func(imageRef string) (string, error)
	// remoteHealthz issues GET /healthz through the remote-access proxy.
	remoteHealthz func(port int, tsLogin string) (int, error)
	// remoteLogin inspects the local OIDC provider on its loopback port.
	remoteLogin func(port int) (loginProbeResult, error)
	// remoteJailLogin asks the hub, from inside the jail, to start a login.
	remoteJailLogin func(ctx context.Context, jr leverexec.Runner, hubURL string) (status int, redirect string, err error)
}

// productionProbes wires the real probes. Host subprocesses (go, node,
// docker) run through r.
func productionProbes(r leverexec.Runner) doctorProbes {
	return doctorProbes{
		dial:            tcpDial,
		goVersion:       func() (string, error) { return goVersionProbe(r) },
		nodeToolchain:   func() (string, error) { return nodeToolchainProbe(r) },
		claudeVersion:   func(imageRef string) (string, error) { return claudeVersionProbe(r, imageRef) },
		remoteHealthz:   remoteHealthzProbe,
		remoteLogin:     remoteLoginProbe,
		remoteJailLogin: remoteJailLoginProbe,
	}
}

// doctorHTTPClient is the client every loopback HTTP probe shares: the proxy
// and its login provider answer on 127.0.0.1, so a short timeout is enough to
// tell "down" from "up".
var doctorHTTPClient = &http.Client{Timeout: 3 * time.Second}

// stateRel renders a state-dir file the way doctor's fix text names it:
// relative to the instance root (".lever-state/remote.log"), never the
// absolute path.
func stateRel(st state.State, path string) string {
	return filepath.Join(filepath.Base(st.Dir), filepath.Base(path))
}

// checkListeningProcess is the pid-then-port ladder shared by the broker and
// remote-proxy checks: a recorded process must exist, be alive, and actually
// listen on addr. It distinguishes three failure modes so the fix is
// unambiguous — never started, died (stale pid), and alive-but-not-serving.
// On success the returned result is the pass line; the caller may keep
// probing and replace it.
func checkListeningProcess(name, pidFile, what, logFile, startFix string, status func() (pid int, found, alive bool), addr string, dial dialFunc) checkResult {
	pid, found, alive := status()
	switch {
	case !found:
		return checkResult{name, false, fmt.Sprintf("no %s — %s was never started (or was cleanly stopped)", pidFile, what), startFix}
	case !alive:
		return checkResult{name, false, fmt.Sprintf("%s names pid %d, but that process is gone (stale pid file)", pidFile, pid), startFix}
	}
	if err := dial(addr); err != nil {
		return checkResult{name, false, fmt.Sprintf("pid %d is alive but nothing is listening on %s", pid, addr), "inspect " + logFile + ", then restart with `lever apply`"}
	}
	return checkResult{name, true, fmt.Sprintf("pid %d, serving on %s", pid, addr), ""}
}

// checkBrokerAlive verifies the recorded broker process is alive AND actually
// listening on the jail port.
func checkBrokerAlive(st state.State, jailPort int, p doctorProbes) checkResult {
	return checkListeningProcess("broker running", "broker.pid", "the broker", stateRel(st, st.Log()),
		"run `lever apply` or `lever up`", func() (int, bool, bool) { return state.PIDStatus(st.PID()) }, fmt.Sprintf("127.0.0.1:%d", jailPort), p.dial)
}

// remoteHealthzProbe issues GET /healthz against the local remote-access
// proxy and returns the response status code.
//
// tsLogin, when non-empty, is sent as Tailscale-User-Login. The proxy's own
// allowed_users gate (remoteproxy.Handler) trusts that header exactly as
// `tailscale serve` would set it for a real request; doctor runs host-side,
// already as trusted as the remote.pat file it just read, so it sets this to
// the first configured allowed user rather than let a pinned instance 403
// its own liveness probe. An unpinned instance (allowed_users empty) sends
// no header at all, matching an ordinary curl/native-client request.
func remoteHealthzProbe(port int, tsLogin string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
	if err != nil {
		return 0, err
	}
	if tsLogin != "" {
		req.Header.Set("Tailscale-User-Login", tsLogin)
	}
	resp, err := doctorHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// loginProbeResult is what remoteLoginProbe observes about the local OIDC
// provider the proxy serves for the hub's login path.
type loginProbeResult struct {
	discovery int    // status of GET /.well-known/openid-configuration
	authorize int    // status of GET /authorize — 404 is the ONLY healthy answer
	authzURL  string // the authorization_endpoint discovery advertises
}

// remoteLoginProbe inspects the local OIDC provider on its loopback port.
//
// It checks the two things that can silently break the login path — discovery
// not being served at all, and the security property the whole design rests
// on: that there is no authorization endpoint. Nothing legitimate ever calls
// /authorize (the proxy drives the login server-side and mints codes
// in-process), so anything but a 404 means this build can mint an
// authorization code over HTTP, on a port every jailed agent can reach.
func remoteLoginProbe(port int) (loginProbeResult, error) {
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var out loginProbeResult

	resp, err := doctorHTTPClient.Get(base + "/.well-known/openid-configuration")
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	out.discovery = resp.StatusCode
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc); err == nil {
		out.authzURL = doc.AuthorizationEndpoint
	}

	aresp, err := doctorHTTPClient.Get(base + "/authorize")
	if err != nil {
		return out, err
	}
	defer aresp.Body.Close()
	out.authorize = aresp.StatusCode
	return out, nil
}

// remoteJailLoginScript asks the hub, FROM INSIDE THE JAIL, to start an OIDC
// login, and prints "<status> <redirect-url>".
//
// -o /dev/null because nothing in the answer's body matters; curl computes
// %{redirect_url} for a 3xx without following it, which is what lets one
// request report both the status and where the hub is sending the browser.
// No Authorization header: the login route is public, and doctor is asking
// what an unauthenticated browser would get.
//
// Absolute path for curl. This runs as the jail's RUN USER, whose PATH has
// run-user-writable directories ahead of /usr/bin — a shim there could answer
// for a login path that does not work.
const remoteJailLoginScript = `exec /usr/bin/curl -sS --connect-timeout 5 --max-time 20 ` +
	`-o /dev/null -w '%{http_code} %{redirect_url}' "$1"`

// remoteJailLoginProbe runs that request and parses its answer.
func remoteJailLoginProbe(ctx context.Context, jr leverexec.Runner, hubURL string) (status int, redirect string, err error) {
	res, err := jr.Run(ctx, nil, "sh", "-c", remoteJailLoginScript, "_", hubURL+"/auth/login/oidc")
	if err != nil {
		return 0, "", fmt.Errorf("%v: %s", err, strings.TrimSpace(res.Stderr))
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return 0, "", fmt.Errorf("no answer from the hub")
	}
	status, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", fmt.Errorf("unparseable answer %q", strings.TrimSpace(res.Stdout))
	}
	if len(fields) > 1 {
		redirect = fields[1]
	}
	return status, redirect, nil
}

// isLoopbackURL reports whether raw addresses the local machine — the test
// that matters for an authorization endpoint, since loopback is what the jail
// is given a route to.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkRemoteLoginPath asks the hub to start a login and reads what comes
// back. It is the ONLY check here that exercises the guest half.
//
// One request proves the whole chain, because of what the hub has to do to
// answer it: getOIDCAuthURL fetches the provider's discovery document before
// it can build a redirect (pkg/hub/oauth.go), and that fetch goes to
// http://127.0.0.1:<login_port> in the GUEST — through the forwarder lever
// installed, to the provider in the proxy process on the host. So a 302 to
// lever's dead authorization endpoint means: the hub read the oidc_login
// block, the forwarder is running, and the provider answered. A host-side
// probe of the provider proves none of that, and would stay green while the
// browser got a 502.
//
// Honest limit: the hub caches discovery for an hour, so a 302 proves the
// chain worked at the time of the FIRST login since the hub started — not
// that it is reachable this second. The forwarder dying after that (a guest
// reboot, say) surfaces on the next cold login, not here.
func checkRemoteLoginPath(ctx context.Context, jr leverexec.Runner, st state.State, p doctorProbes) (detail string, fix string, ok bool) {
	if jr == nil {
		return "", "", true // no jail transport wired (tests)
	}
	status, redirect, err := p.remoteJailLogin(ctx, jr, scionpkg.DefaultHubEndpoint)
	switch {
	case err != nil:
		return fmt.Sprintf("could not ask the hub to start a login from inside the jail: %v", err),
			"is the machine up? `lever apply`", false
	case status == http.StatusBadRequest:
		return "the hub does not have lever's OIDC login configured (it refused to start one)",
			"run `lever apply` — it writes the oidc_login block into the jail's ~/.scion/settings.yaml and restarts the hub so it is read", false
	case status == http.StatusInternalServerError:
		return "the hub could not reach lever's login provider (it failed to build an authorization URL)",
			"the jail cannot reach the provider on the host: re-run `lever apply` to reinstall and restart the forwarder. " +
				"On `egress: closed`, a login port granted since the instance came up needs `lever down` + `lever up` — a live " +
				"closed chain is deliberately never rebuilt in place (internal/backend/guest.ApplyEgress, the I2 property)", false
	case status != http.StatusFound:
		return fmt.Sprintf("the hub answered %d when asked to start a login, want 302", status),
			"inspect " + stateRel(st, st.RemoteLog()), false
	case !strings.HasPrefix(redirect, remoteproxy.DeadAuthorizationEndpoint):
		return fmt.Sprintf("the hub starts logins against %q, not lever's provider", redirect),
			"another OIDC provider is configured in the jail's ~/.scion/settings.yaml — remove it and re-run `lever apply`", false
	}
	return "login path reaches the provider through the jail", "", true
}

// checkRemote verifies the remote-access proxy (`lever remote`) when
// configured on: the recorded process is alive and actually listening on
// EffectiveRemotePort (the same pid/dial split as checkBrokerAlive), the
// injected PAT is present and not group/other-accessible, the login path the
// proxy depends on is intact, and — last — an end-to-end GET /healthz THROUGH
// the proxy returns 200, proving the whole chain (loopback listener ->
// origin/identity gates -> hub web session -> PAT injection -> hub) actually
// works, not just that a process happens to be running.
//
// /healthz is deliberately the LAST probe. It is not an API path, so the proxy
// opens a hub web session before forwarding it: the probe therefore drives a
// full server-side OIDC handshake, and fails 502 whenever the login path is
// broken. Running it ahead of the login checks reported that 502 instead of
// the specific, actionable failure. Side effect worth knowing: `lever doctor`
// performs a REAL login, so the hub creates (or reuses) a user row for the
// identity the probe carries — allowed_users[0], or lever's unnamed operator
// when the list is empty (remoteproxy.identityFor) — exactly as that
// operator's first browser visit would.
//
// Disabled is a pass, not a warning: remote access is opt-in and most
// instances never turn it on. PAT EXPIRY is deliberately not checked here —
// the hub stores it and lever has no cheap read for it in v1; the repair
// (delete remote.pat, then `lever apply`) is the same shape as the
// documented controller-PAT re-mint, default expiry 90d (see the
// remote-access guide).
func checkRemote(ctx context.Context, app *config.App, st state.State, p doctorProbes, jr leverexec.Runner) checkResult {
	const name = "remote access"
	if !app.RemoteEnabled() {
		return checkResult{name, true, "disabled", ""}
	}
	const applyFix = "run `lever apply`"
	remoteLog := stateRel(st, st.RemoteLog())
	port := app.EffectiveRemotePort()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	alive := checkListeningProcess(name, "remote.pid", "the proxy", remoteLog, applyFix,
		func() (int, bool, bool) { return state.PIDStatus(st.RemotePID()) }, addr, p.dial)
	if !alive.ok {
		return alive
	}
	fi, err := os.Stat(st.RemotePAT())
	switch {
	case err != nil:
		return checkResult{name, false, "remote.pat is missing", applyFix}
	case fi.Mode().Perm()&0o077 != 0:
		return checkResult{name, false, fmt.Sprintf("remote.pat has mode %04o (group/other-accessible, want 0600)", fi.Mode().Perm()), "chmod 600 " + st.RemotePAT()}
	}
	// The login checks run BEFORE the end-to-end /healthz probe, and that
	// order is load-bearing. /healthz is not an API path, so the proxy opens a
	// hub web session for it (remoteproxy.NewHandler's `cfg.Session != nil &&
	// !isAPIPath` gate) — a broken login chain therefore makes healthz answer
	// 502 too. Probing healthz first SHADOWED the precise diagnosis: the
	// operator was told "GET /healthz returned 502 — inspect remote.log" when
	// the one actionable message ("a login port granted since the instance came
	// up needs `lever down` + `lever up`") was the very next check.
	loginPort := app.EffectiveRemoteLoginPort()
	login, err := p.remoteLogin(loginPort)
	switch {
	case err != nil:
		return checkResult{name, false, fmt.Sprintf("the login provider on 127.0.0.1:%d is unreachable: %v", loginPort, err),
			"inspect " + remoteLog + " — without it the hub cannot log the browser in, and the web UI stays at 401"}
	case login.discovery != http.StatusOK:
		return checkResult{name, false, fmt.Sprintf("the login provider answered %d to OIDC discovery, want 200", login.discovery),
			"inspect " + remoteLog}
	case login.authorize != http.StatusNotFound:
		// Loud on purpose: a provider that answers /authorize can mint an
		// authorization code over HTTP, and every jailed agent can reach that
		// port through the guest forwarder (lever maps guest loopback into
		// each agent's netns). See remoteproxy.Provider.handleAuthorize.
		return checkResult{name, false, fmt.Sprintf("the login provider answered %d to GET /authorize, want 404 — it must have NO authorization endpoint", login.authorize),
			"this is a security defect in this lever build, not a configuration problem: stop the proxy (`lever stop`) and report it"}
	case isLoopbackURL(login.authzURL):
		// The property, not one port: an authorization endpoint anywhere on
		// loopback is one the JAIL can reach (guest loopback is mapped into
		// every agent netns), and reaching one means minting a code. lever's
		// advertised endpoint names a host that does not resolve.
		return checkResult{name, false, fmt.Sprintf("OIDC discovery advertises an authorization endpoint on loopback (%s), which the jail can reach", login.authzURL),
			"this is a security defect in this lever build, not a configuration problem: stop the proxy (`lever stop`) and report it"}
	}
	if detail, fix, ok := checkRemoteLoginPath(ctx, jr, st, p); !ok {
		return checkResult{name, false, detail, fix}
	}

	// Last, because it depends on everything above: this is the only probe
	// that goes end to end through the proxy to the hub.
	status, err := p.remoteHealthz(port, firstOrEmpty(app.Remote.AllowedUsers))
	if err != nil {
		return checkResult{name, false, fmt.Sprintf("GET /healthz through the proxy failed: %v", err), "inspect " + remoteLog + " — the hub may be down, or the proxy misconfigured"}
	}
	if status != http.StatusOK {
		return checkResult{name, false, fmt.Sprintf("GET /healthz through the proxy returned %d, want 200", status), "inspect " + remoteLog + " and " + stateRel(st, st.RemoteAudit())}
	}
	return checkResult{name, true, alive.detail + fmt.Sprintf(", PAT present, healthz OK, login provider on 127.0.0.1:%d (no authorization endpoint), hub login path reaches it", loginPort), ""}
}

// firstOrEmpty returns the first element of ss, or "" when ss is empty.
func firstOrEmpty(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// checkToolBackends verifies every broker tool is reachable/spawnable up
// front: external tools must be listening on their loopback backend, and
// supervised tools must have their command resolvable on the supervisor PATH
// (a not-on-PATH supervised tool fails silently at spawn otherwise). Config
// validation already rejects an unresolvable supervised command, so a config
// that loaded is expected to pass the resolution half here — this check is the
// operator-facing confirmation and the external-liveness probe.
func checkToolBackends(tools []config.Tool, p doctorProbes) checkResult {
	const name = "tool backends"
	var down []string
	probed := 0
	for _, t := range tools {
		probed++
		if t.External {
			addr := backendHostPort(t.Backend)
			if err := p.dial(addr); err != nil {
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

// sharedDirLister returns the shared directories the hub records for a project.
// Injected so the check is unit-testable without a hub.
type sharedDirLister func(ctx context.Context, project string) ([]hubapi.SharedDir, error)

// hubAnswered reports whether the hub replied and lever could not use the
// reply, as opposed to lever never reaching the hub at all.
func hubAnswered(err error) bool {
	var apiErr *hubapi.APIError
	return errors.As(err, &apiErr)
}

// checkProjectSharedDirs reports any directory the hub mounts into every agent
// of the project. scion stamps a writable `scratchpad` on every new project
// (scion#925), which is a read/write channel between the manager and every
// worker — the opposite of lever's subtree isolation — so apply strips it. An
// entry here means the strip did not run, or something re-added one.
//
// An unreachable hub is NOT a finding: a stopped instance already shows up in
// the broker check, and a second red line would only add noise. Anything the
// hub actually ANSWERED is different — a 403, a 401, or a project the hub does
// not list means the check cannot do its job for a reason the operator must
// fix, so that fails. The detail always states which case it was, so a pass is
// never mistaken for a clean verdict.
//
// The check reads the hub's project record. An agent that started before the
// directory was removed keeps its bind mount until it restarts, so a clean line
// here is a statement about new agents.
func checkProjectSharedDirs(ctx context.Context, project string, list sharedDirLister) checkResult {
	const name = "project shared directories"
	if list == nil {
		return checkResult{name, true, "not checked", ""}
	}
	dirs, err := list(ctx, project)
	if err != nil {
		if answered := hubAnswered(err); answered {
			return checkResult{name, false,
				"could not read the project's shared directories: " + err.Error(),
				"the hub answered, so this is not a down instance — check the controller PAT " +
					"(`.lever-state/`) and that the hub knows a project named " + project}
		}
		return checkResult{name, true, "not checked (hub not reachable): " + err.Error(), ""}
	}
	if len(dirs) == 0 {
		return checkResult{name, true,
			"none in the hub record — no directory is shared into newly started agents", ""}
	}
	var names []string
	for _, d := range dirs {
		if d.ReadOnly {
			names = append(names, d.Name+" (read-only)")
			continue
		}
		names = append(names, d.Name)
	}
	return checkResult{name, false,
		fmt.Sprintf("the hub mounts %d shared director(y/ies) into EVERY agent of %s: %s",
			len(dirs), project, strings.Join(names, ", ")),
		"run `lever apply` to strip scion's default scratchpad; remove any other entry with " +
			"`DELETE /api/v1/projects/<project-uuid>/shared-dirs/<name>` against the hub"}
}

// agentRoleLister returns the hub's agent records for a project. Injected so
// the check is unit-testable without a hub.
type agentRoleLister func(ctx context.Context, project string) ([]hubapi.Agent, error)

// checkAgentRoles reports agent records that store no authorization role.
//
// The role is written when an agent is CREATED (scion#1089) and is immutable
// after, so a record made by an older scion carries none — and scion#1102
// resolves an unset stored role to FULL, at dispatch and on every token
// refresh. `scion resume` takes no --role flag, so nothing repairs such a
// record: the only route is to delete the agent and lose its conversation.
//
// That makes the verdict depend on the installed scion, not just the records.
// On a roles-aware scion an unrolled record is a live promotion and fails. On
// an older one the same record is harmless — failing there would cry wolf on
// every pre-#1089 instance — but this is the one place an operator can learn,
// BEFORE bumping the pin, that the bump will promote them.
//
// An unreachable hub is not a finding (the broker check already covers a
// stopped instance); a hub that ANSWERED unusably is, exactly as for shared
// directories.
func checkAgentRoles(ctx context.Context, project string, rolesSupported func(context.Context) (bool, error), list agentRoleLister) checkResult {
	const name = "agent authorization roles"
	if list == nil || rolesSupported == nil {
		return checkResult{name, true, "not checked", ""}
	}
	agents, err := list(ctx, project)
	if err != nil {
		if hubAnswered(err) {
			return checkResult{name, false,
				"could not read the hub's agent records: " + err.Error(),
				"the hub answered, so this is not a down instance — check the controller PAT " +
					"(`.lever-state/`) and that the hub knows a project named " + project}
		}
		return checkResult{name, true, "not checked (hub not reachable): " + err.Error(), ""}
	}
	if len(agents) == 0 {
		return checkResult{name, true, "no agent records yet", ""}
	}

	var unrolled, held []string
	for _, a := range agents {
		if a.Role == "" {
			unrolled = append(unrolled, a.Slug)
			continue
		}
		held = append(held, a.Slug+"="+a.Role)
	}
	if len(unrolled) == 0 {
		return checkResult{name, true,
			fmt.Sprintf("%d record(s), all carry a stored role: %s", len(agents), strings.Join(held, ", ")), ""}
	}

	// Only now does the installed scion matter, so only now is it probed — a
	// scion that cannot answer must not turn a clean instance into a finding.
	roles, perr := rolesSupported(ctx)
	if perr != nil {
		return checkResult{name, true,
			fmt.Sprintf("not checked (cannot tell whether this scion understands roles: %v); %d record(s) store none: %s",
				perr, len(unrolled), strings.Join(unrolled, ", ")), ""}
	}
	if !roles {
		return checkResult{name, true,
			fmt.Sprintf("%d record(s) store no role: %s — harmless on this scion, which predates roles (scion#1089), "+
				"but a pin at or after scion#1102 resolves an unset role to FULL, and `lever up` will then refuse to resume them",
				len(unrolled), strings.Join(unrolled, ", ")), ""}
	}
	return checkResult{name, false,
		fmt.Sprintf("%d record(s) store no role while this scion resolves that to FULL hub authority: %s",
			len(unrolled), strings.Join(unrolled, ", ")),
		"a stored role is immutable and `scion resume` cannot set one — delete each agent so lever recreates it with " +
			"--role baseline (its conversation is LOST), or pin a scion older than scion#1089"}
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

// goVersionProbe resolves and runs `go version` on the host PATH. It
// distinguishes "not on PATH at all" from "on PATH but broken" (e.g. a dead
// asdf/mise shim, which typically fails with exit status 126) by resolving via
// exec.LookPath first.
func goVersionProbe(r leverexec.Runner) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := r.Run(ctx, nil, goBin, "version")
	if err != nil {
		return "", fmt.Errorf("%s version: %w", goBin, err)
	}
	return res.Stdout, nil
}

// checkGoToolchain verifies a real, working Go toolchain is resolvable on
// PATH when scion needs to be cross-compiled (source checkout or a pinned
// module version) — `lever up`/`apply` shell out to `go` for that build. A
// broken shim (e.g. asdf/mise without the version installed) fails with an
// opaque "exit status 126" deep inside apply; this turns it into an
// up-front, actionable diagnosis. No build requested => no go needed => pass.
func checkGoToolchain(scion config.ScionConfig, p doctorProbes) checkResult {
	const name = "go toolchain"
	if scion.Source == "" && scion.Version == "" {
		return checkResult{name, true, "scion build not required", ""}
	}
	out, err := p.goVersion()
	if err != nil {
		return checkResult{name, false, "go toolchain not usable: " + err.Error(),
			`put a REAL Go toolchain on PATH (not just an asdf/mise shim), e.g. export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"; ` + "`go version` should print"}
	}
	return checkResult{name, true, strings.TrimSpace(out), ""}
}

// nodeToolchainProbe resolves and validates node+npm for the scion web-asset
// build, returning the node version.
//
// It probes inside the build's own cache directory, and creates that directory
// to do so. A version manager that resolves node by walking UP for a project
// file (asdf, mise) gives different answers in different directories, so a
// probe run in the user's project — which may have its own .tool-versions —
// would not be evidence about the build, which runs elsewhere. Same reason
// scionbin.FetchModule resolves the real go binary rather than trusting a shim.
func nodeToolchainProbe(r leverexec.Runner) (string, error) {
	root, err := webassets.CacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create web build cache %s: %w", root, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return webassets.CheckNodeToolchain(ctx, r, root)
}

// checkNodeToolchain verifies node+npm can build scion's web UI when the
// instance serves it (`remote.enabled` on a source/version scion). Without
// this, a missing or broken toolchain surfaces either as a failed `lever apply`
// deep in npm or — worse, before the build existed — as scion's bare "Web UI
// Not Available" page in the browser, long after the cause. No UI to build =>
// no node needed => pass.
func checkNodeToolchain(app *config.App, p doctorProbes) checkResult {
	const name = "node toolchain"
	if !app.ScionWebAssets() {
		return checkResult{name, true, "scion web UI build not required", ""}
	}
	version, err := p.nodeToolchain()
	if err != nil {
		fix := ""
		if errors.Is(err, webassets.ErrNodeToolchain) {
			fix = webassets.NodeToolchainFix
		}
		return checkResult{name, false, err.Error(), fix}
	}
	return checkResult{name, true, "node " + strings.TrimSpace(version), ""}
}

// checkOperatorSkills verifies the framework skills scaffolded by `lever init`
// are present, current for this lever version, and unmodified — or adopted as
// an accepted baseline via `lever init --adopt` — and referenced from the
// tree-root CLAUDE.md. Runs the scaffold engine in check (read-only) mode.
// Drift PAST an adopted baseline is called out separately: the scaffolds live
// inside the agent-writable tree, so unexplained change there is the tamper
// signal this check exists for.
func checkOperatorSkills(app *config.App, stateDir state.State) checkResult {
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
		// An adoption is an owner choice, but it silently pins the file to the
		// framework baseline it was adopted AT — an instance can upgrade many
		// versions past a (possibly security-relevant) skill change while
		// doctor reports healthy (#16, reproduced live: a 0.3.1 adoption
		// surviving to 0.8.1). Version drift on an adopted file is therefore a
		// FAILING check: visibility only, never an auto-overwrite.
		var lagging []string
		for _, r := range results {
			if r.Action == skillAdopted && r.AdoptedVersion != Version {
				v := r.AdoptedVersion
				if v == "" {
					v = "unknown"
				}
				lagging = append(lagging, fmt.Sprintf("%s (baseline %s)", r.RelPath, v))
			}
		}
		if len(lagging) > 0 {
			return checkResult{name, false,
				fmt.Sprintf("adopted skill baseline lags framework %s: %s — missing framework guidance added since adoption", Version, strings.Join(lagging, "; ")),
				"review the drift vs the current scaffold, merge what you want to keep, set `lever-version: " + Version + "` in the file's frontmatter (your attestation of the baseline reviewed against), then re-bless with `lever init --adopt` — or reclaim the framework version with `lever init --force`"}
		}
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
func checkDirectives(app *config.App, st state.State) checkResult {
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
	_, found, alive := state.PIDStatus(st.PID())
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
func checkAgentCert(st state.State, now time.Time) checkResult {
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

// claudeVersionProbe reads the baked Claude Code version from an image's
// `claude_code_version` label via `docker image inspect`. The image ID
// inspect in internal/jail (hostImageID) reads a different field and is not
// exported, so this is its own invocation.
func claudeVersionProbe(r leverexec.Runner, imageRef string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Run(ctx, nil, "docker", "image", "inspect",
		"--format", `{{index .Config.Labels "claude_code_version"}}`, imageRef)
	if err != nil {
		if msg := strings.TrimSpace(res.Stderr + res.Stdout); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	v := strings.TrimSpace(res.Stdout)
	if v == "<no value>" { // label absent
		v = ""
	}
	return v, nil
}

// checkClaudeVersion reports the Claude Code version baked into the manager
// image. It reads a label (no container run). A missing label means a
// pre-label image and is reported informationally, not as a failure; an
// inspect error (image not built/loaded) is a real fault.
func checkClaudeVersion(imageRef string, p doctorProbes) checkResult {
	const name = "agent claude version"
	v, err := p.claudeVersion(imageRef)
	if err != nil {
		return checkResult{name, false, "could not inspect image " + imageRef + ": " + err.Error(),
			"build/load the agent image (`lever apply`) before this check can read its baked version"}
	}
	if v == "" {
		return checkResult{name, true, "no claude_code_version label on " + imageRef + " (pre-label image; rebuild to record it)", ""}
	}
	return checkResult{name, true, "baked " + v + " in " + imageRef + " (running containers keep their version until recreated: `lever stop && lever up`)", ""}
}
