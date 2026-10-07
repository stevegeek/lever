package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/agentledger"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/remoteproxy"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/webpush"
	"github.com/stevegeek/lever/internal/wire"
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
	// claudeVersion reads the baked Claude Code version label of an image
	// from the host docker store; claudeVersionTar reads it from the docker
	// archive the image ships in (image_tar), for a host with no docker.
	claudeVersion    func(imageRef string) (string, error)
	claudeVersionTar func(tarPath, imageRef string) (string, error)
	// leverVersion and leverVersionTar read the image's lever_version label
	// the same two ways.
	leverVersion    func(imageRef string) (string, error)
	leverVersionTar func(tarPath, imageRef string) (string, error)
	// remoteHealthz issues GET /healthz through the remote-access proxy.
	remoteHealthz func(healthzProbe) (int, error)
	// remoteLogin inspects the local OIDC provider on its loopback port.
	remoteLogin func(port int) (loginProbeResult, error)
	// remoteJailLogin asks the hub, from inside the jail, to start a login.
	remoteJailLogin func(ctx context.Context, jr proc.Runner, hubURL string) (status int, redirect string, err error)
}

// productionProbes wires the real probes. Host subprocesses (go, node,
// docker) run through r.
func productionProbes(r proc.Runner) doctorProbes {
	return doctorProbes{
		dial:          tcpDial,
		goVersion:     func() (string, error) { return goVersionProbe(r) },
		nodeToolchain: func() (string, error) { return nodeToolchainProbe(r) },
		claudeVersion: func(imageRef string) (string, error) { return imageLabelProbe(r, imageRef, claudeVersionLabel) },
		claudeVersionTar: func(tarPath, imageRef string) (string, error) {
			return jail.ImageTarLabel(tarPath, imageRef, claudeVersionLabel)
		},
		leverVersion: func(imageRef string) (string, error) { return imageLabelProbe(r, imageRef, leverVersionLabel) },
		leverVersionTar: func(tarPath, imageRef string) (string, error) {
			return jail.ImageTarLabel(tarPath, imageRef, leverVersionLabel)
		},
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

// stateDirName is the instance-relative name of the state directory, so
// user-facing text never hardcodes it.
func stateDirName() string { return state.DirName }

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

// healthzProbe is what remoteHealthzProbe sends: where to dial, and the
// identity to carry.
type healthzProbe struct {
	// Addr is the host:port to dial (config.App.RemoteProbeAddr): loopback
	// by default, the bind address when the proxy listens elsewhere.
	Addr string
	// Port is the proxy's port. The request's Host is always
	// 127.0.0.1:<Port>, whatever Addr is: that is the name the proxy's Host
	// gate admits for host-side probes (remoteproxy.hostAllowed).
	Port int
	// Header and Login: when Login is non-empty it is sent in Header, the
	// configured identity header.
	Header, Login string
}

// remoteHealthzProbe issues GET /healthz against the local remote-access
// proxy and returns the response status code.
//
// Login, when non-empty, is sent in the configured identity header. The
// proxy's own allowed_users gate (remoteproxy.Handler) trusts that header
// exactly as the front would set it for a real request; doctor runs
// host-side, already as trusted as the remote.pat file it just read, so it
// sets this to the first configured allowed user rather than let a pinned
// instance 403 its own liveness probe. An unpinned instance (allowed_users
// empty) sends no header at all, matching an ordinary curl/native-client
// request.
func remoteHealthzProbe(p healthzProbe) (int, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+p.Addr+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	req.Host = fmt.Sprintf("127.0.0.1:%d", p.Port)
	if p.Login != "" {
		req.Header.Set(p.Header, p.Login)
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
func remoteJailLoginProbe(ctx context.Context, jr proc.Runner, hubURL string) (status int, redirect string, err error) {
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
func checkRemoteLoginPath(ctx context.Context, jr proc.Runner, st state.State, p doctorProbes) (detail string, fix string, ok bool) {
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
// remote PAT `lever apply` mints is present and not group/other-accessible
// (the proxy no longer sends it — every request rides the operator's hub
// session — but apply still mints it, and a world-readable token is still a
// leak), the login path the proxy depends on is intact, and — last — an
// end-to-end GET /healthz THROUGH the proxy returns 200, proving the whole
// chain (loopback listener -> origin/identity gates -> hub web session ->
// hub) actually works, not just that a process happens to be running.
//
// /healthz is deliberately the LAST probe. The proxy opens a hub web session
// before forwarding any request: the probe therefore drives a
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
func checkRemote(ctx context.Context, app *config.App, st state.State, p doctorProbes, jr proc.Runner) checkResult {
	const name = "remote access"
	if !app.RemoteEnabled() {
		return checkResult{name, true, "disabled", ""}
	}
	const applyFix = "run `lever apply`"
	remoteLog := stateRel(st, st.RemoteLog())
	port := app.EffectiveRemotePort()
	addr := app.RemoteProbeAddr()
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
	// order is load-bearing. The proxy opens a hub web session for every
	// request it forwards, /healthz included — a broken login chain therefore
	// makes healthz answer 502 too. Probing healthz first SHADOWED the precise diagnosis: the
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
	status, err := p.remoteHealthz(healthzProbe{Addr: addr, Port: port,
		Header: app.EffectiveRemoteIdentityHeader(), Login: firstOrEmpty(app.Remote.Logins())})
	if err != nil {
		return checkResult{name, false, fmt.Sprintf("GET /healthz through the proxy failed: %v", err), "inspect " + remoteLog + " — the hub may be down, or the proxy misconfigured"}
	}
	if status != http.StatusOK {
		return checkResult{name, false, fmt.Sprintf("GET /healthz through the proxy returned %d, want 200", status), "inspect " + remoteLog + " and " + stateRel(st, st.RemoteAudit())}
	}
	return checkResult{name, true, alive.detail + fmt.Sprintf(", PAT present, healthz OK, login provider on 127.0.0.1:%d (no authorization endpoint), hub login path reaches it", loginPort), ""}
}

// checkRemoteExposure is a warning row per remote setting that loads but
// weakens a default protection (config.App.RemoteWarnings): a non-loopback
// bind, and trust_forwarded_host. Neither is a fault, and lever cannot see the
// host firewall they rest on, so the row neither passes silently nor fails
// the run — it restates what the operator must keep true. Nothing to say is a
// plain pass.
func checkRemoteExposure(app *config.App) checkResult {
	const name = "remote exposure"
	if !app.RemoteEnabled() {
		return checkResult{name, true, "remote access disabled", ""}
	}
	warnings := app.RemoteWarnings()
	if len(warnings) == 0 {
		return checkResult{name, true, fmt.Sprintf("loopback-only listener (%s), identity from %s", app.RemoteListenAddr(), app.EffectiveRemoteIdentityHeader()), ""}
	}
	return warnResult(name, strings.Join(warnings, "; "),
		"confirm only the authenticating front can reach "+app.RemoteListenAddr()+" and that it overwrites "+
			app.EffectiveRemoteIdentityHeader()+"; see the remote-access guide, non-Tailscale fronts")
}

// checkVerifiedChat reports whether agents can verify web chat (package
// chatledger): the proxy records only for verified logins, and the ledger
// proves something only while no jail can write it.
func checkVerifiedChat(app *config.App, st state.State) checkResult {
	const name = "verified chat"
	switch {
	case !app.RemoteEnabled():
		return checkResult{name, true, "remote access disabled (no web chat)", ""}
	case brokerctl.StateInsideTree(app, st):
		return checkResult{name, false, "off: the state directory " + st.Dir + " is inside the tree " + app.Tree +
			", which agents mount, so a ledger there proves nothing",
			"point `tree:` at a subdirectory that does not contain " + stateDirName() + "/"}
	case len(app.Remote.AllowedUsers) == 0:
		return warnResult(name, "off: remote.allowed_users is empty, so the proxy verifies no login and records no chat; "+
			"agents cannot verify a web chat post and treat every one as data (they do not act on it or reply)",
			"list the operator's login in remote.allowed_users, then run `lever apply` and `lever init`")
	}
	p := brokerctl.ChatLedgerPath(app, st)
	// Every allowed login speaks with operator authority once verified: say
	// who, so a login added only to look at the web UI is not a surprise.
	var tiers []string
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			reach := strings.Join(u.Agents, ", ")
			if len(u.See) > 0 {
				reach += "; see: " + strings.Join(u.See, ", ")
			}
			tiers = append(tiers, u.Login+" contact ("+reach+")")
		} else {
			tiers = append(tiers, u.Login+" operator")
		}
	}
	tier := "tiers: " + strings.Join(tiers, "; ")
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return checkResult{name, true, "on (" + tier + "); no chat post recorded yet (" + stateRel(st, p) + "/)", ""}
	}
	if err != nil {
		return checkResult{name, false, "cannot read the chat ledger: " + err.Error(), "check " + stateRel(st, p)}
	}
	fix := "remove " + p + "; the remote proxy writes a new one"
	if !fi.IsDir() {
		return checkResult{name, false, "the chat ledger " + stateRel(st, p) + " is not a directory (a symlink?), so agents get no answer", fix}
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return checkResult{name, false, fmt.Sprintf("the chat ledger %s is %v: another user can add a file, so agents get no answer", stateRel(st, p), perm), "chmod 700 " + p}
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return checkResult{name, false, fmt.Sprintf("the chat ledger %s belongs to uid %d, not to you (uid %d)", stateRel(st, p), owner, os.Getuid()), fix}
	}
	files, _ := os.ReadDir(p)
	return checkResult{name, true, fmt.Sprintf("on (%s); ledger %s/ (%d files, 0700)", tier, stateRel(st, p), len(files)), ""}
}

// agentMsgsLive is what the running processes were started with, for the
// agent messages row: the running broker's config hash (ok false when no
// broker answers) and whether a running remote proxy's stamp differs from
// this config.
type agentMsgsLive struct {
	brokerHash func() (string, bool)
	// proxyMatches reports whether a remote proxy runs, and whether it was
	// started by this lever version with the remote config whose hash is
	// given.
	proxyMatches func(hash string) (running, matches bool)
}

// liveAgentMessages reads what the running broker and remote proxy were
// started with: the broker's /epoch config hash on its admin port, and the
// proxy's stamp file against this config.
func liveAgentMessages(ctx context.Context, app *config.App, st state.State) agentMsgsLive {
	return agentMsgsLive{
		brokerHash: func() (string, bool) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var er wire.EpochResponse
			u := fmt.Sprintf("http://127.0.0.1:%d%s", app.EffectiveAdminPort(), wire.PathEpoch)
			// A redirect is not the broker's answer: never followed.
			client := &http.Client{Timeout: 2 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			if err := httpjson.Get(ctx, client, u, &er); err != nil {
				return "", false
			}
			return er.ConfigHash, true
		},
		proxyMatches: func(hash string) (bool, bool) {
			_, found, alive := state.PIDStatus(st.RemotePID())
			if !found || !alive {
				return false, false
			}
			return true, st.RemoteStampMatches(cli.VersionString(), hash)
		},
	}
}

// checkAgentMessages reports remote.agent_messages: off, or on with the
// limits, the contacts and their agents, and the agent ledger's safety. The
// broker and the proxy read the config only when they start. So while the
// setting is on, the row says when either runs with another config (or
// lever version); while it is off, it says so only when a process runs
// with this config plus agent messages on, the one case where the setting
// itself differs. Other config changes are other rows' business.
func checkAgentMessages(app *config.App, st state.State, live agentMsgsLive) checkResult {
	r := agentMessagesRow(app, st)
	if !r.ok {
		return r
	}
	on := app.AgentMessagesOn()
	// The hashes a process started with agent messages on would carry.
	onApp := *app
	onApp.Remote.AgentMessages.Enabled = true
	var stale []string
	if live.brokerHash != nil {
		if h, ok := live.brokerHash(); ok && (on && h != brokerctl.ConfigHash(app) || !on && onApp.AgentMessagesOn() && h == brokerctl.ConfigHash(&onApp)) {
			stale = append(stale, "broker")
		}
	}
	if live.proxyMatches != nil {
		if on {
			if running, ok := live.proxyMatches(brokerctl.RemoteConfigHash(app)); running && !ok {
				stale = append(stale, "remote proxy")
			}
		} else if onApp.AgentMessagesOn() {
			if running, ok := live.proxyMatches(brokerctl.RemoteConfigHash(&onApp)); running && ok {
				stale = append(stale, "remote proxy")
			}
		}
	}
	if len(stale) > 0 {
		return warnResult(r.name, r.detail+"; the running "+strings.Join(stale, " and ")+
			" started with another config; agent message settings take effect after `lever init` + `lever apply`",
			"run `lever init`, then `lever apply`")
	}
	return r
}

func agentMessagesRow(app *config.App, st state.State) checkResult {
	const name = "agent messages"
	if !app.AgentMessagesOn() {
		return checkResult{name, true, "off (agents answer contacts as before; nothing is filtered)", ""}
	}
	if brokerctl.StateInsideTree(app, st) {
		return checkResult{name, false, "on, but the state directory is inside the tree: no record can be kept, so contacts see no agent message",
			"point `tree:` at a subdirectory that does not contain " + stateDirName() + "/"}
	}
	var who []string
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			who = append(who, u.Login+" ("+strings.Join(u.Agents, ", ")+")")
		}
	}
	detail := fmt.Sprintf("on: follow_up_after %s, max_chars %d; contacts: %s; agents need an image with this release's lever-agent (contacts, contact_message)",
		shortDuration(app.EffectiveAgentFollowUpAfter()), app.EffectiveAgentMaxChars(), strings.Join(who, "; "))
	p := st.AgentLedger()
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return checkResult{name, true, detail + "; no message authorized yet", ""}
	case err != nil:
		return checkResult{name, false, "cannot read the agent ledger: " + err.Error(), "check " + stateRel(st, p)}
	case !fi.IsDir():
		return checkResult{name, false, "the agent ledger " + stateRel(st, p) + " is not a directory (a symlink?), so contacts see no agent message", "remove " + p}
	case fi.Mode().Perm()&0o022 != 0:
		return checkResult{name, false, fmt.Sprintf("the agent ledger %s is %v: another user can add a file, so contacts see no agent message", stateRel(st, p), fi.Mode().Perm()), "chmod 700 " + p}
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return checkResult{name, false, fmt.Sprintf("the agent ledger %s belongs to uid %d, not to you (uid %d)", stateRel(st, p), owner, os.Getuid()), "remove " + p}
	}
	// Every authorization counts the hourly rate over all contact files,
	// so one unsafe file refuses every authorization (failing closed).
	if bad := unsafeLedgerFile(p); bad != "" {
		return checkResult{name, false, "the agent ledger file " + stateRel(st, bad) + " is not a private regular file of yours: " +
			"every agent's contact_message is refused (unavailable) and contacts are shown no agent message until it is fixed",
			"chmod 600 " + bad + " (or remove it, which forgets that contact's records)"}
	}
	return checkResult{name, true, detail + "; ledger " + stateRel(st, p) + "/ (0700)", ""}
}

// unsafeLedgerFile is the first contact file in dir (or its .1) that is not
// a regular file, is writable by others, or belongs to another user; "" if
// none.
func unsafeLedgerFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !agentledger.IsContactFile(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
			return p
		}
		if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
			return p
		}
	}
	return ""
}

// shortDuration prints whole hours as "24h" (time.Duration prints 24h0m0s).
func shortDuration(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}

// checkChatLabels reads remote.labels_file the way the proxy does. A bad
// file is a warning, not a failure: it is agent-written, and its only
// effect is that the chat page shows no labels. The row names the fault.
func checkChatLabels(app *config.App) checkResult {
	const name = "chat labels"
	rel := app.Remote.LabelsFile
	if !app.RemoteEnabled() || rel == "" {
		return checkResult{name, true, "not set (the chat page shows no labels)", ""}
	}
	labels, err := remoteproxy.ReadLabels(app.Tree, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return checkResult{name, true, rel + " absent (no labels)", ""}
	}
	if err != nil {
		return warnResult(name, "labels ignored: "+rel+" "+labelFault(err),
			"the manager writes "+rel+" as a JSON object {\"<agent>\": \"<label>\"}: a regular file of at most 16 KiB, reached through no symbolic link")
	}
	known := 0
	for n := range labels {
		if n == app.Name || slices.ContainsFunc(app.Workers, func(w config.Worker) bool { return w.Name == n }) {
			known++
		}
	}
	return checkResult{name, true, fmt.Sprintf("%d label(s) for known agents in %s (%d other name(s) ignored)", known, rel, len(labels)-known), ""}
}

// labelFault names a labels-file fault in fixed words: those are what the
// operator acts on.
func labelFault(err error) string {
	switch {
	case errors.Is(err, fsutil.ErrSymlink):
		return "is reached through a symbolic link"
	case errors.Is(err, fsutil.ErrFileTooLarge):
		return "is larger than 16 KiB"
	case errors.Is(err, fsutil.ErrNotRegularFile):
		return "is not a regular file"
	case errors.Is(err, fsutil.ErrEscapesTree):
		return "is not inside the tree"
	case errors.Is(err, remoteproxy.ErrLabelsShape):
		return "is not a JSON object of strings"
	}
	return "cannot be read: " + firstLine(err.Error())
}

// checkPush reports remote.push the way the proxy reads it. The proxy
// writes push/status.json at start and after each send, so this row shows
// the last outcome and whether the running proxy pushes to test hosts.
func checkPush(app *config.App, st state.State) checkResult {
	const name = "push"
	if !app.PushOn() {
		return checkResult{name, true, "off (no service worker, no push routes, no hub streams)", ""}
	}
	if brokerctl.StateInsideTree(app, st) {
		return checkResult{name, false, "on, but the state directory is inside the tree: push stays off (agents could read its key)",
			"point `tree:` at a subdirectory that does not contain " + stateDirName() + "/"}
	}
	if th := app.Remote.Push.TestHosts; len(th) > 0 {
		return checkResult{name, false, "on, with remote.push.test_hosts set (" + strings.Join(th, ",") + "): TEST ONLY, the proxy may push over plain http to loopback",
			"remove remote.push.test_hosts, stop the proxy, unset " + webpush.TestHostsEnv + ", and run `lever apply`"}
	}
	dir := st.PushDir()
	sum := remoteproxy.ReadPushSummary(dir)
	switch {
	case errors.Is(sum.KeyErr, fs.ErrNotExist):
		return warnResult(name, "on, but no key yet: the remote proxy creates "+stateRel(st, dir)+"/vapid.key when it starts", "run `lever apply`")
	case errors.Is(sum.KeyErr, webpush.ErrBadKey):
		return checkResult{name, false, "on, but " + sum.KeyErr.Error() + " (empty after a power loss?); push is off",
			"stop the proxy, remove " + filepath.Join(dir, "vapid.key") + ", and run `lever apply`: the proxy creates a new key, and every device must turn notifications on again"}
	case sum.KeyErr != nil:
		return checkResult{name, false, "on, but the push key is unusable: " + sum.KeyErr.Error() + "; push is off", "chmod 600 " + filepath.Join(dir, "vapid.key") + " (or remove it: every device must then turn notifications on again)"}
	case errors.Is(sum.StoreErr, remoteproxy.ErrBadPushStore):
		return checkResult{name, false, "on, but " + sum.StoreErr.Error() + " (empty after a power loss?); push is off",
			"stop the proxy, remove " + filepath.Join(dir, "subscriptions.json") + ", and run `lever apply`: a device that turned notifications on sends its subscription again when its page loads"}
	case sum.StoreErr != nil:
		return checkResult{name, false, "on, but the subscription store is unusable: " + sum.StoreErr.Error() + "; push is off", "chmod 600 " + filepath.Join(dir, "subscriptions.json") + " (or remove it)"}
	}
	total, per := 0, []string{}
	for _, l := range slices.Sorted(maps.Keys(sum.Subs)) {
		total += sum.Subs[l]
		per = append(per, fmt.Sprintf("%s %d", l, sum.Subs[l]))
	}
	last := "none yet"
	if sum.Last != nil && sum.Last.Result != "started" {
		last = fmt.Sprintf("%s %s (%s)", sum.Last.Result, sum.Last.Host, sum.Last.At.Local().Format("2006-01-02 15:04"))
	}
	detail := fmt.Sprintf("on (subject %s): key present (0600); %d subscription(s)%s; last send: %s; "+
		"the proxy connects out to fcm.googleapis.com, *.push.apple.com, updates.push.services.mozilla.com and *.notify.windows.com on 443",
		app.Remote.Push.Subject, total, func() string {
			if len(per) == 0 {
				return ""
			}
			return " (" + strings.Join(per, ", ") + ")"
		}(), last)
	if sum.Last != nil && sum.Last.TestHosts {
		return checkResult{name, false, detail + "; the running proxy pushes to test hosts (TEST ONLY)",
			"stop the proxy, unset " + webpush.TestHostsEnv + ", and run `lever apply`"}
	}
	if sum.Last != nil && sum.Last.Result == "failed" {
		return warnResult(name, detail, "see "+stateRel(st, st.RemoteAudit())+" (decision push-failed)")
	}
	return checkResult{name, true, detail, ""}
}

// warnResult is a warning row: not a failure (doctor's exit status ignores
// it), but printed with its fix. It is a passing checkResult that carries a
// fix — no passing check sets one otherwise — which printDoctorReport renders
// with "!" instead of "✓".
func warnResult(name, detail, fix string) checkResult {
	return checkResult{name, true, detail, fix}
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
func checkScionProject(st types.ScionProjectState, mountDest string) checkResult {
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
					"(`" + stateDirName() + "/`) and that the hub knows a project named " + project}
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
					"(`" + stateDirName() + "/`) and that the hub knows a project named " + project}
		}
		return checkResult{name, true, "not checked (hub not reachable): " + err.Error(), ""}
	}
	if len(agents) == 0 {
		return checkResult{name, true, "no agent records yet", ""}
	}

	// A grandfathered role is scion's migration writing `full` where nothing
	// was stored: the same hazard as an unrolled record, and named as such
	// rather than listed among the healthy ones.
	var unrolled, held []string
	for _, a := range agents {
		switch {
		case a.RoleGrandfathered:
			unrolled = append(unrolled, a.Slug+" (grandfathered to "+a.Role+")")
		case a.Role == "":
			unrolled = append(unrolled, a.Slug)
		default:
			held = append(held, a.Slug+"="+a.Role)
		}
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

// agentSessionReader reads an agent's session from inside its container
// (jail.AgentProbe in production): its hub token expiry and whether its
// harness still runs. Both answers are the agent's own word; doctor turns
// them into a verdict and a time, and never prints what the container said.
type agentSessionReader interface {
	HubToken(ctx context.Context, ref string) (jail.HubTokenTimes, error)
	HarnessAlive(ctx context.Context, ref string) (bool, error)
}

// agentLister reads scion's agent records for the in-jail project: phase and
// container status, which the hub's own record lacks (hubapi.Agent).
type agentLister func(ctx context.Context, project string) ([]scionpkg.Agent, error)

// checkManagerLive reports whether the manager agent is actually up: a record
// exists, its phase is running, its container is live, and — since lever#34 —
// its harness is still turning. Before lever#31 none of doctor's checks read
// the manager at all, so fourteen green rows could sit over an instance with
// no manager container — "doctor passes" meant "the plumbing is fine", never
// "the agent works". This is the one row that says the second thing.
//
// The activity is the hub-side state scion's Claude Code hooks report
// (scion.Activity*), shown with the age of its last change. A harness that
// cannot complete a turn — no guest DNS (lever#34), an expired credential, an
// API outage — keeps a live container and a running phase. Crashed and
// offline fail the row. Stalled does not: the hub's stall sweeper marks a
// stuck harness stalled after its threshold (default 5 min), but it marks an
// idle manager the same way, so the row passes and names both readings (the
// `guest DNS` row is the one that fails on the lever#34 cause). A long
// working stays green: real work looks the same from here, and `lever
// attach` is the way to tell. A list error is "not checked" (a down jail or hub is another
// check's finding), never a pass.
//
// Two faults hide behind a live container, and the session reader (nil = not
// probed) exposes both. An expired agent hub token fails the row whatever
// the activity: sciontool's refresh timer stands still while the host
// sleeps, an expired token cannot refresh itself, and the hub's stall
// sweeper then marks the manager stalled — the very reading that passes
// above. A phase stopped over a container that is still up is told apart by
// whether claude still runs: another claude process in the container firing
// the shared SessionEnd hook leaves the harness running (apply reports it
// running again), a claude that exited does not.
func checkManagerLive(ctx context.Context, project, name string, list agentLister, session agentSessionReader, now time.Time) checkResult {
	const check = "manager agent"
	if list == nil {
		return checkResult{check, true, "not checked", ""}
	}
	agents, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	a := scionpkg.FindAgent(agents, name)
	if a == nil {
		return checkResult{check, false, fmt.Sprintf("no record for manager %q — nothing is running this instance", name),
			"run `lever up`"}
	}
	ref := jail.ContainerName(hubProjectKey(project), name)
	if a.Phase == "running" && scionpkg.ContainerLive(a.ContainerStatus) {
		if session != nil {
			if tok, err := session.HubToken(ctx, ref); err == nil && tok.Expired() {
				return checkResult{check, false,
					fmt.Sprintf("%q is running (container %s; %s), but its hub token expired at %s (%s ago by the guest clock): every reply, status update and heartbeat it sends fails with 401 — the agent's token refresh timer stands still while the host sleeps, and an expired token cannot refresh itself",
						name, scionpkg.ContainerLabel(a.ContainerStatus), activityAge(a, now), tok.Expiry.Format(time.RFC3339), tok.Now.Sub(tok.Expiry).Truncate(time.Second)),
					hubTokenFix(name, project)}
			}
		}
		if a.Activity == scionpkg.ActivityStalled {
			// The hub's stall sweeper also marks a manager that sits idle at
			// its prompt: after a turn that ended without a waiting-for-input
			// report, the record reads working, then stalled. That is the
			// normal state of an instance nobody talked to for a while, so it
			// cannot fail the row (it failed scripted deploy gates on idle
			// instances). A turn that never finishes looks the same from
			// here; the detail says how to tell them apart.
			return checkResult{check, true,
				fmt.Sprintf("%q is running (container %s; %s) — idle at its prompt, or a turn that never finished: `lever attach` shows which (a stuck LLM call ends in `Request timed out`; then check the `guest DNS` and credential rows)",
					name, scionpkg.ContainerLabel(a.ContainerStatus), activityAge(a, now)), ""}
		}
		if scionpkg.ActivityDead(a.Activity) {
			return checkResult{check, false,
				fmt.Sprintf("manager %q has a live container but its harness is %s — it is not completing turns", name, activityAge(a, now)),
				"`lever attach` shows the harness (a stuck LLM call ends in `Request timed out`); on lima check the `guest DNS` row (lever#34), then the credential row; `lever stop` and `lever up` restart the harness"}
		}
		return checkResult{check, true, fmt.Sprintf("%q is running (container %s; %s)", name, scionpkg.ContainerLabel(a.ContainerStatus), activityAge(a, now)), ""}
	}
	if a.Phase == "running" && a.ContainerStatus == "" {
		// The container column is refreshed by the runtime broker's heartbeat,
		// not computed at read time; a blank is "cannot tell", which is what
		// `lever up` reads it as too (apply.ObserveManagerLive), never a
		// death and never a pass with a claim attached.
		return checkResult{check, true, fmt.Sprintf("%q is running; not checked further (no container status reported yet)", name), ""}
	}
	if a.Phase == scionpkg.PhaseStopped && scionpkg.ContainerLive(a.ContainerStatus) && session != nil {
		alive, err := session.HarnessAlive(ctx, ref)
		switch {
		case err == nil && alive:
			return checkResult{check, false,
				fmt.Sprintf("manager %q's session ended in the hub (phase stopped), but claude still runs in its container (%s) — another claude process in the container (such as `claude mcp list` through podman exec) fired the shared SessionEnd hook; the hub refuses `lever attach`, and a resume would start a new session (no --continue)",
					name, scionpkg.ContainerLabel(a.ContainerStatus)),
				"run `lever apply`: it has the agent report its session running again, and the conversation continues (`lever stop` does the same before it suspends)"}
		case err == nil:
			return checkResult{check, false,
				fmt.Sprintf("manager %q's claude has exited (phase stopped; container %s)", name, scionpkg.ContainerLabel(a.ContainerStatus)),
				stoppedResumeFix}
		}
	}
	fix := "run `lever up` to resume it"
	if a.Phase == scionpkg.PhaseStopped {
		fix = stoppedResumeFix
	}
	if a.Phase == "error" {
		fix = "run `lever up` (an error-phase record is resumed with --force; if that fails the record and its conversation are kept, and `lever up --fresh` is the way to discard them) — its container log in the guest holds the harness's last output"
	}
	return checkResult{check, false,
		fmt.Sprintf("manager %q is not live: phase %s, container %s", name, scionpkg.BoundedQuote(a.Phase), scionpkg.BoundedQuote(a.ContainerStatus)), fix}
}

// stoppedResumeFix is the fix for a manager whose session really ended:
// scion's hub resumes a stopped record with a fresh harness session (only a
// suspended one gets --continue), so the operator hears how to get the old
// conversation back.
const stoppedResumeFix = "run `lever up` — scion resumes a stopped record in a new claude session (no --continue); the old conversation stays in the agent home: `lever attach`, then `/resume`"

// hubTokenFix names the heal for an agent whose hub token expired.
func hubTokenFix(name, project string) string {
	return "run `lever apply` — it runs `scion reset-auth` for each running agent whose hub token expired (no restart; the conversation is kept). By hand, in the guest: `scion reset-auth " + name + " -g " + project + "` with the controller PAT"
}

// checkAgentHubTokens reads the hub token of the manager and of every
// configured worker whose container is live, and fails on any that expired:
// replies, status updates and heartbeats from that agent all fail with 401,
// while its container, phase and (for the manager) activity row stay green
// or merely "stalled". A token past its refresh point (2 h before expiry) is
// a warning: sciontool's refresh timer counts the guest's monotonic clock,
// which stands still while the host sleeps, so an overdue refresh turns into
// an expiry unless it fires in time. An agent whose token cannot be read is
// "not checked" for that agent, never a pass with a claim.
//
// Only a running or stopped agent fails the row: those are the phases
// `lever apply` heals. An expired token under another phase (error,
// starting, …) is a warning, since the resume or restart that phase needs
// issues a new token anyway. A running manager's expired token is the
// manager row's failure already, so here it is a warning pointing there:
// one fault, one failed row.
func checkAgentHubTokens(ctx context.Context, project, manager string, agents []string, list agentLister, session agentSessionReader) checkResult {
	const check = "agent hub tokens"
	if list == nil || session == nil {
		return checkResult{check, true, "not checked", ""}
	}
	recs, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var good, overdue, expired, otherPhase, unread []string
	managerExpired := false
	var fixFor string
	for _, name := range agents {
		a := scionpkg.FindAgent(recs, name)
		if a == nil || !scionpkg.ContainerLive(a.ContainerStatus) {
			continue
		}
		tok, err := session.HubToken(ctx, jail.ContainerName(hubProjectKey(project), name))
		switch {
		case err != nil:
			unread = append(unread, name)
		case tok.Expired() && name == manager && a.Phase == scionpkg.PhaseRunning:
			managerExpired = true
		case tok.Expired() && (a.Phase == scionpkg.PhaseRunning || a.Phase == scionpkg.PhaseStopped):
			expired = append(expired, fmt.Sprintf("%s (expired %s ago)", name, tok.Now.Sub(tok.Expiry).Truncate(time.Second)))
			if fixFor == "" {
				fixFor = name
			}
		case tok.Expired():
			otherPhase = append(otherPhase, fmt.Sprintf("%s (expired, phase %s)", name, scionpkg.BoundedQuote(scionpkg.PhaseLabel(a.Phase))))
		case tok.RefreshOverdue():
			overdue = append(overdue, fmt.Sprintf("%s (refresh due %s ago, expires in %s)", name,
				tok.Now.Sub(tok.Expiry.Add(-jail.AgentTokenRefreshMargin)).Truncate(time.Second), tok.Expiry.Sub(tok.Now).Truncate(time.Second)))
		default:
			good = append(good, fmt.Sprintf("%s (valid %s)", name, tok.Expiry.Sub(tok.Now).Truncate(time.Minute)))
		}
	}
	notChecked := ""
	if len(unread) > 0 {
		notChecked = "; not checked: " + strings.Join(unread, ", ") + " (token unreadable)"
	}
	var warns, fixes []string
	if managerExpired {
		warns = append(warns, manager+"'s token expired (the manager agent row reports it)")
		fixes = append(fixes, hubTokenFix(manager, project))
	}
	if len(otherPhase) > 0 {
		warns = append(warns, "expired under a phase `lever apply` does not heal: "+strings.Join(otherPhase, ", "))
		fixes = append(fixes, "bring the agent back to running (`lever up` for the manager, a resume for a worker): the resume issues it a new token")
	}
	if len(overdue) > 0 {
		warns = append(warns, "refresh overdue: "+strings.Join(overdue, ", ")+" — sciontool's refresh timer stands still while the host sleeps; it may still fire in time")
		fixes = append(fixes, "if the token expires first, `lever apply` resets it (`scion reset-auth`, no restart)")
	}
	switch {
	case len(expired) > 0:
		detail := "expired: " + strings.Join(expired, ", ") + " — every reply, status update and heartbeat from these agents fails with 401, and an expired token cannot refresh itself"
		if len(warns) > 0 {
			detail += "; also " + strings.Join(warns, "; ")
		}
		return checkResult{check, false, detail + notChecked, hubTokenFix(fixFor, project)}
	case len(warns) > 0:
		return warnResult(check, strings.Join(warns, "; ")+notChecked, strings.Join(fixes, "; "))
	case len(good) == 0 && len(unread) == 0:
		return checkResult{check, true, "no running agent", ""}
	case len(good) == 0:
		return checkResult{check, true, strings.TrimPrefix(notChecked, "; "), ""}
	}
	return checkResult{check, true, strings.Join(good, ", ") + notChecked, ""}
}

// guestDNSProbeName is the name doctor resolves from inside the guest: the
// one every subscription-mode agent must reach on its first turn.
const guestDNSProbeName = "api.anthropic.com"

// checkGuestDNS resolves a public name from inside the guest, through the
// same resolver path the agent containers use. Before lever#34 a guest with
// no DNS looked exactly like a healthy idle one — `lever up` succeeded and
// every doctor row was green, because they prove the containers are up, not
// that a lookup completes — while the agent's every LLM call timed out (on
// lima, LEVER_EGRESS dropped the LIMADNS DNAT to the host alias). Closed
// egress drops DNS by design (agents dial the broker by IP), so the row is
// informational there. The probe is bounded: a dead resolver path makes
// getent wait on systemd-resolved's own retries.
func checkGuestDNS(ctx context.Context, closedEgress bool, jr proc.Runner) checkResult {
	const check = "guest DNS"
	if closedEgress {
		return checkResult{check, true, "not checked (closed egress drops DNS by design; agents dial the broker by IP)", ""}
	}
	res, err := jr.Run(ctx, nil, "timeout", "10", "getent", "ahosts", guestDNSProbeName)
	if err == nil && strings.TrimSpace(res.Stdout) != "" {
		return checkResult{check, true, guestDNSProbeName + " resolves in the guest", ""}
	}
	why := "no answer"
	if err != nil {
		why = firstLine(err.Error())
	}
	if res.Code == 124 {
		why = "lookup timed out after 10s"
	}
	return checkResult{check, false,
		fmt.Sprintf("the guest cannot resolve %s (%s) — an agent's every LLM call will time out while every other row stays green", guestDNSProbeName, why),
		"in the guest, `sudo iptables -L LEVER_EGRESS -v -n` shows which DROP the lookups hit; on lima the resolver path is the LIMADNS DNAT to the host alias, which `lever apply` ACCEPTs in the open posture (lever#34) — re-run `lever apply`, then `lever up`"}
}

// activityAge renders an agent's activity with the age of its last change:
// "activity completed, 3m0s ago", or without the age when the hub reported
// no event time, or "no activity reported" when the field is empty.
func activityAge(a *scionpkg.Agent, now time.Time) string {
	if a.Activity == "" {
		return "no activity reported"
	}
	// The activity is the agent's own report: only a known label is shown.
	activity := scionpkg.ActivityLabel(a.Activity)
	if a.LastActivityEvent.IsZero() {
		return "activity " + activity
	}
	return fmt.Sprintf("activity %s, %s ago", activity, now.Sub(a.LastActivityEvent).Truncate(time.Second))
}

// checkManagerImage compares the image the manager record was created with
// against manager.image. A record keeps its image for life — `lever up`
// resumes it unchanged, and only a create reads the config — so after a
// manager.image edit the version row (which inspects the CONFIGURED image)
// reads green while the manager runs something else (lever#33). Podman may
// report the record's image qualified (docker.io/, or the localhost/ alias
// the load step adds), so refs are compared normalised. Informational when
// there is nothing to compare: no record (the manager row already fails),
// a record without an image field, or no listing at all.
func checkManagerImage(ctx context.Context, project, name, want string, list agentLister) checkResult {
	const check = "manager image"
	if list == nil || want == "" {
		return checkResult{check, true, "not checked", ""}
	}
	agents, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	a := scionpkg.FindAgent(agents, name)
	if a == nil {
		return checkResult{check, true, "not checked (no manager record)", ""}
	}
	if a.Image == "" {
		return checkResult{check, true, "not checked (the record reports no image)", ""}
	}
	if jail.SameImageRef(a.Image, want) {
		return checkResult{check, true, fmt.Sprintf("%q runs %s, as configured", name, a.Image), ""}
	}
	return checkResult{check, false,
		fmt.Sprintf("manager %q was created on %s but manager.image is now %s — a resume keeps the record's image", name, a.Image, want),
		"run `lever up --fresh` to recreate the manager on the configured image (the conversation is discarded)"}
}

// networkCheckedAgents is every agent the agent-network row inspects: the
// manager (whose scion slug is the instance name) and each declared worker.
func networkCheckedAgents(manager string, workers []string) []string {
	return append([]string{manager}, workers...)
}

// netModeLister returns the network mode of a jail container by id or name
// (jail.ContainerNetworkMode in production); jail.ErrNoContainer when there
// is none.
type netModeLister func(ctx context.Context, ref string) (string, error)

// checkAgentNetwork finds agent containers that do not run in their own
// pasta netns (lever#35). On podman 4.9 (the Lima Ubuntu 24.04 guest) a
// container created without lever's drop-in runs slirp4netns, where
// host.containers.internal resolves to the guest's own address, which
// LEVER_EGRESS drops: the agent cannot reach the hub (heartbeats, `scion
// message`, token refresh all fail). The mode is fixed at create; scion
// deletes and recreates a stopped agent's container on start, so a stop and
// an up fix it and keep the conversation. The container is found by the id
// scion reports or else by scion's container name, as for the ticket mounts.
func checkAgentNetwork(ctx context.Context, project string, agents []string, forceHost bool, list agentLister, modes netModeLister) checkResult {
	const check = "agent network"
	if forceHost {
		// The escape hatch runs every agent on host networking on purpose.
		return checkResult{check, true, "not checked (" + jail.ForceHostNetworkEnv + " is set: agents share the guest's network)", ""}
	}
	if list == nil || modes == nil {
		return checkResult{check, true, "not checked", ""}
	}
	records, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var wrong, unchecked []string
	checked := 0
	for _, name := range agents {
		a := scionpkg.FindAgent(records, name)
		if a == nil {
			continue
		}
		ref := a.ContainerID
		if ref == "" {
			ref = jail.ContainerName(hubProjectKey(project), name)
		}
		mode, err := modes(ctx, ref)
		if errors.Is(err, jail.ErrNoContainer) {
			continue
		}
		if err != nil {
			unchecked = append(unchecked, name)
			continue
		}
		checked++
		if mode != "pasta" {
			wrong = append(wrong, name+" ("+mode+")")
		}
	}
	if len(wrong) > 0 {
		return checkResult{check, false,
			fmt.Sprintf("agent %s not in its own pasta netns: it cannot reach the hub at host.containers.internal", braceList(wrong)),
			"run `lever apply` (writes the pasta drop-in), then `lever stop` and `lever up`: scion recreates each stopped container, and the manager conversation is kept"}
	}
	if len(unchecked) > 0 {
		return checkResult{check, true, fmt.Sprintf("could not inspect %s", braceList(unchecked)), ""}
	}
	if checked == 0 {
		return checkResult{check, true, "no agent containers to check", ""}
	}
	return checkResult{check, true, fmt.Sprintf("%d agent container(s) run pasta", checked), ""}
}

// mountLister returns the in-container mount points of a jail container by
// id or name (jail.ContainerMountTargets in production); jail.ErrNoContainer
// when there is none.
type mountLister func(ctx context.Context, ref string) ([]string, error)

// workerTicketMount is where a worker's container sees its enrolment ticket
// directory (the broker's read-only volume; see broker.WorkerSpec.TicketDir).
const workerTicketMount = "/run/lever"

// checkWorkerTicketMounts finds worker records created BEFORE the guest
// ticket channel (0.22): scion keeps a record's volumes for life, so such a
// worker has no /run/lever mount, and its next re-enrolment (a resume after
// its leaf lapsed, the healer's bounce) cannot find a ticket — the broker no
// longer stages one in the tree. The fix is a purge and a re-dispatch, which
// creates the container with the mount. The container is found by the id
// scion reports or, when the pin reports none (89ed0fe8 does not), by
// scion's container name. Workers with no record or no container are
// skipped; a listing or inspect failure is "not checked", never a pass.
func checkWorkerTicketMounts(ctx context.Context, project string, workers []string, list agentLister, mounts mountLister) checkResult {
	const check = "worker ticket mounts"
	if len(workers) == 0 {
		return checkResult{check, true, "no workers declared", ""}
	}
	if list == nil || mounts == nil {
		return checkResult{check, true, "not checked", ""}
	}
	agents, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var stale, unchecked []string
	checked := 0
	for _, name := range workers {
		a := scionpkg.FindAgent(agents, name)
		if a == nil {
			continue
		}
		ref := a.ContainerID
		if ref == "" {
			ref = jail.ContainerName(hubProjectKey(project), name)
		}
		targets, err := mounts(ctx, ref)
		if errors.Is(err, jail.ErrNoContainer) {
			continue
		}
		if err != nil {
			unchecked = append(unchecked, name)
			continue
		}
		checked++
		if !slices.Contains(targets, workerTicketMount) {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		return checkResult{check, false,
			fmt.Sprintf("worker %s was created before the guest ticket channel (no %s mount): its next enrolment cannot find a ticket",
				braceList(stale), workerTicketMount),
			fmt.Sprintf("run `lever worker purge %s --force`, then dispatch it again from the manager (its work product in the tree is kept)", braceList(stale))}
	}
	if len(unchecked) > 0 {
		return checkResult{check, true, fmt.Sprintf("not checked for worker %s (could not inspect its container)", braceList(unchecked)), ""}
	}
	if checked == 0 {
		return checkResult{check, true, "no worker container to inspect", ""}
	}
	return checkResult{check, true, fmt.Sprintf("%d worker container(s) mount %s", checked, workerTicketMount), ""}
}

// recordVolumeReader returns the extra mounts an agent's hub record was
// created with (hubRecordVolumes in production).
type recordVolumeReader func(ctx context.Context, project, agent string) ([]jail.Mount, error)

// mountInspector returns a jail container's mounts with their writability by
// id or name (jail.ContainerMounts in production); jail.ErrNoContainer when
// there is none.
type mountInspector func(ctx context.Context, ref string) ([]jail.Mount, error)

// checkManagerReadOnly verifies that the manager container holds every
// mount of its manager.read_only plan (apply.ManagerTreeMountGaps, shared
// with apply's keep/resume warning): each entry mounted read-only at its
// place in the workspace; each pin — an entry's ancestors, and every worker
// dir with its ancestors — mounted (its writability is not the point; that
// it is a mount point, which cannot be renamed away, is); each from the
// same directory of the in-jail tree (project). scion keeps a record's
// volumes for life, so a manager created before an entry was added has no
// such mount; only a fresh create gives it the mounts.
//
// The inspect is not enough on its own: when the host replaces a protected
// directory after the create, podman still lists the read-only mount while
// a write from the container lands in the new directory (verified on
// OrbStack 2026-10-04). So for a RUNNING container every entry is also
// probed live (apply.ProbeReplacedEntries); a probe that cannot run, or a
// container that is not running, is a warning that says so — never a pass.
// The container is found by the id scion reports or else by scion's
// container name, as for the worker ticket mounts. No manager record or
// container, a listing or an inspect failure, is "not checked".
//
// The row also reports the other direction (apply.ManagerStaleTreeMounts):
// a mount the record still holds for a directory the config dropped, or
// whose directory is gone from the host tree — the latter fails, because
// the next resume cannot recreate the container (card #157). That half
// runs even with read_only unset, since removing the whole list is how a
// record ends up with mounts the config no longer names; only then is a
// failed listing or inspect still "none configured".
//
// With no container to inspect (or an inspect that fails), that half reads
// the hub record's volumes instead (record; nil skips it), which a resume
// recreates the container from; the plan half then stays "not checked".
func checkManagerReadOnly(ctx context.Context, project, tree, name string, want []config.TreeMount, list agentLister, inspect mountInspector, record recordVolumeReader, probe apply.WritableProbe) checkResult {
	const check = "manager read-only paths"
	notChecked := func(detail string) checkResult {
		if len(want) == 0 {
			detail = "none configured"
		}
		return checkResult{check, true, detail, ""}
	}
	if list == nil || inspect == nil {
		return notChecked("not checked")
	}
	agents, err := list(ctx, project)
	if err != nil {
		return notChecked("not checked (could not list agents): " + firstLine(err.Error()))
	}
	a := scionpkg.FindAgent(agents, name)
	if a == nil {
		return notChecked("not checked (no manager record)")
	}
	ref := a.ContainerID
	if ref == "" {
		ref = jail.ContainerName(hubProjectKey(project), name)
	}
	mounts, err := inspect(ctx, ref)
	if err != nil {
		if record != nil {
			if vols, rerr := record(ctx, hubProjectKey(project), name); rerr == nil {
				if stale := apply.ManagerStaleTreeMounts(project, tree, want, vols); !stale.Empty() {
					r := staleTreeMountsResult(check, name, stale)
					r.detail += " (read from the hub record: no manager container to inspect)"
					return r
				}
			}
		}
		if errors.Is(err, jail.ErrNoContainer) {
			return notChecked("not checked (no manager container)")
		}
		return notChecked("not checked (could not inspect the manager container): " + firstLine(err.Error()))
	}
	stale := apply.ManagerStaleTreeMounts(project, tree, want, mounts)
	if len(want) == 0 {
		if !stale.Empty() {
			return staleTreeMountsResult(check, name, stale)
		}
		return checkResult{check, true, "none configured", ""}
	}
	r := managerReadOnlyGaps(ctx, check, project, name, ref, a.ContainerStatus, want, mounts, probe)
	if stale.Empty() {
		return r
	}
	sr := staleTreeMountsResult(check, name, stale)
	if r.fix == "" {
		return sr
	}
	// Both: the row fails if either does, and names both, with both fixes.
	return checkResult{check, r.ok && sr.ok, r.detail + "; also " + stale.String(), r.fix + "; and " + sr.fix}
}

// managerReadOnlyGaps is checkManagerReadOnly's half for the current plan:
// every planned mount present, and (on a running container) every entry
// refusing a write.
func managerReadOnlyGaps(ctx context.Context, check, project, name, ref, containerStatus string, want []config.TreeMount, mounts []jail.Mount, probe apply.WritableProbe) checkResult {
	gaps := apply.ManagerTreeMountGaps(project, want, mounts)
	if gaps.Empty() {
		if !scionpkg.ContainerLive(containerStatus) {
			return warnResult(check, "the mounts are listed, but the write probe did not run (the manager container is not running), so a protected directory replaced on the host would go unnoticed",
				"bring the manager up (`lever up`) and re-run doctor")
		}
		if probe == nil {
			// A warning row needs a fix to print as one ("!"), never as "✓".
			return warnResult(check, "the mounts are listed, but the write probe is not wired, so a protected directory replaced on the host would go unnoticed",
				"this is a lever wiring gap; check the manager by hand (`podman exec <container> test -w <entry>` in the guest must fail) and report it")
		}
		replaced, err := apply.ProbeReplacedEntries(ctx, probe, ref, want)
		if err != nil {
			return warnResult(check, "the mounts are listed, but the write probe could not run ("+firstLine(err.Error())+"), so protection is not confirmed",
				"re-run doctor once the manager container is up")
		}
		gaps.Replaced = replaced
	}
	if !gaps.Empty() {
		return checkResult{check, false, fmt.Sprintf("manager %q: %s", name, gaps), apply.ManagerTreeMountsFix(gaps)}
	}
	entries, pins := 0, 0
	for _, w := range want {
		if w.ReadOnly {
			entries++
		} else {
			pins++
		}
	}
	return checkResult{check, true, fmt.Sprintf("%d path(s) read-only in %q (mounted, and a write probe refused), %d pin(s) mounted (pins are checked by inspect only)", entries, name, pins), ""}
}

// staleTreeMountsResult is checkManagerReadOnly's row for mounts the
// record holds beyond the config: a gone directory fails (the next resume
// fails), a dropped mount warns.
func staleTreeMountsResult(check, name string, stale apply.StaleTreeMounts) checkResult {
	detail := fmt.Sprintf("manager %q: %s", name, stale)
	if len(stale.Gone) > 0 {
		return checkResult{check, false, detail, stale.Fix()}
	}
	return warnResult(check, detail, stale.Fix())
}

// checkWorkerTreeBootstraps finds a bootstrap.json under a worker's own
// tree (<tree>/<worker dir>/.lever/bootstrap.json). Since the guest ticket
// channel (0.22) lever stages a worker's ticket only in the guest runtime
// directory, mounted read-only at /run/lever in that worker alone; a copy in
// the tree is one an older lever left or an agent wrote, readable by the
// manager, and it hid a worker-side `lever-manager` that read the wrong path
// (it worked only where such a copy sat). dirs maps a worker to its host
// directory.
func checkWorkerTreeBootstraps(tree string, dirs map[string]string) checkResult {
	const check = "worker tree bootstraps"
	if len(dirs) == 0 {
		return checkResult{check, true, "no workers declared", ""}
	}
	var found, paths, linked []string
	for name, dir := range dirs {
		p := filepath.Join(dir, ".lever", "bootstrap.json")
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		found = append(found, name)
		// The tree is agent-writable. A path that goes through a symlink
		// (a `.lever` link planted in a worker tree, or a worker directory
		// the manager replaced with a link) names a file somewhere else, so
		// no `rm` line is printed for it: pasting one would delete the
		// link's target, which can be the manager's own bootstrap.
		if real, err := filepath.EvalSymlinks(p); err != nil || real != underRealTree(tree, p) {
			linked = append(linked, name)
			continue
		}
		paths = append(paths, shellQuote(p))
	}
	if len(found) == 0 {
		return checkResult{check, true, "no bootstrap.json under a worker tree", ""}
	}
	slices.Sort(found)
	slices.Sort(paths)
	slices.Sort(linked)
	detail := fmt.Sprintf("worker %s has a bootstrap.json in its own tree: a worker's ticket belongs only in its read-only %s mount, never in the agent-writable tree",
		braceList(found), workerTicketMount)
	var fix []string
	if len(paths) > 0 {
		fix = append(fix, "look first (`ls -la` on its `.lever` directory: the tree is agent-writable and can change after this check), then delete it: rm -- "+strings.Join(paths, " "))
	}
	if len(linked) > 0 {
		detail += fmt.Sprintf("; for worker %s the path goes through a symbolic link, so it names a file outside that worker's tree", braceList(linked))
		fix = append(fix, fmt.Sprintf("for worker %s do NOT delete through the path: look at the `.lever` entry (and the worker directory) with `ls -la`, and remove the link itself", braceList(linked)))
	}
	return checkResult{check, false, detail, strings.Join(fix, "; ")}
}

// shellQuote quotes s for a POSIX shell: single quotes, with each single
// quote in s written as '\”. Nothing is expanded inside single quotes, so a
// pasted line cannot run a `$(…)` or a backtick that a path contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// underRealTree is where p is when nothing below the instance tree is a
// symlink: the tree's own real location plus p's path inside it. The tree
// root is the mount point and not an agent's to replace; everything below it
// is agent-writable. Comparing this with EvalSymlinks(p) tells whether any
// component below the tree (or the file itself) is a link, while a tree that
// sits behind a link on the host (/tmp or /var on macOS) does not count.
func underRealTree(tree, p string) string {
	real, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return ""
	}
	rest, err := filepath.Rel(tree, p)
	if err != nil || strings.HasPrefix(rest, "..") {
		return ""
	}
	return filepath.Join(real, rest)
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
func goVersionProbe(r proc.Runner) (string, error) {
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
func nodeToolchainProbe(r proc.Runner) (string, error) {
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
		// surviving to 0.8.1). cli.Version drift on an adopted file is therefore a
		// FAILING check: visibility only, never an auto-overwrite.
		var lagging []string
		for _, r := range results {
			if r.Action == skillAdopted && r.AdoptedVersion != cli.Version {
				v := r.AdoptedVersion
				if v == "" {
					v = "unknown"
				}
				lagging = append(lagging, fmt.Sprintf("%s (baseline %s)", r.RelPath, v))
			}
		}
		if len(lagging) > 0 {
			return checkResult{name, false,
				fmt.Sprintf("adopted skill baseline lags framework %s: %s — missing framework guidance added since adoption", cli.Version, strings.Join(lagging, "; ")),
				"review the drift vs the current scaffold, merge what you want to keep, set `lever-version: " + cli.Version + "` in the file's frontmatter (your attestation of the baseline reviewed against), then re-bless with `lever init --adopt` — or reclaim the framework version with `lever init --force`"}
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

// The labels image/lever-claude/Dockerfile bakes from build args: the pinned
// Claude Code version, and the lever version the in-jail binaries were built
// from.
const (
	claudeVersionLabel = "claude_code_version"
	leverVersionLabel  = "lever_version"
)

// imageLabelProbe reads one label of an image in the host docker store via
// `docker image inspect`; "" when the image has no such label. The image ID
// inspect in internal/jail (hostImageID) reads a different field and is not
// exported, so this is its own invocation.
func imageLabelProbe(r proc.Runner, imageRef, label string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Run(ctx, nil, "docker", "image", "inspect",
		"--format", `{{index .Config.Labels "`+label+`"}}`, imageRef)
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
// image. It reads a label (no container run) — from the archive when the
// image ships as one (tarPath, from image_tar), else from host docker. A
// missing label means a pre-label image and is reported informationally, not
// as a failure; an inspect error (image not built/loaded, tar unreadable or
// mistagged) is a real fault.
func checkClaudeVersion(imageRef, tarPath string, p doctorProbes) checkResult {
	const name = "agent claude version"
	source := imageRef
	var v string
	var err error
	if tarPath != "" {
		source = imageRef + " (from " + tarPath + ")"
		v, err = p.claudeVersionTar(tarPath, imageRef)
	} else {
		v, err = p.claudeVersion(imageRef)
	}
	if err != nil {
		return checkResult{name, false, "could not inspect image " + source + ": " + err.Error(),
			"build/load the agent image (`lever apply`) before this check can read its baked version"}
	}
	if v == "" {
		return checkResult{name, true, "no claude_code_version label on " + source + " (pre-label image; rebuild to record it)", ""}
	}
	return checkResult{name, true, "baked " + v + " in " + source + " (a manager keeps the image it was created on until recreated: `lever up --fresh`; the manager image row compares the two)", ""}
}

// checkLeverVersion compares the lever version baked into the manager image
// (its lever_version label, set by `make lever-image`) with this binary's.
// The image tag names only the arch, so a build from another lever source
// replaces it with nothing else to show for it (lever#18). host is the
// release (cli.Version) and hostFull what `lever version` prints; the label
// holds the latter form. Releases are compared, not commits: a host built
// from a later commit of the same release is not a finding, and the detail
// names both commits.
//
// A missing label (an image older than the label, or one not built by `make
// lever-image`) and an image that cannot be read are informational: the
// claude-version row already fails on an unreadable image.
func checkLeverVersion(imageRef, tarPath, host, hostFull string, p doctorProbes) checkResult {
	const name = "agent lever version"
	source := imageRef
	var v string
	var err error
	if tarPath != "" {
		source = imageRef + " (from " + tarPath + ")"
		v, err = p.leverVersionTar(tarPath, imageRef)
	} else {
		v, err = p.leverVersion(imageRef)
	}
	if err != nil {
		return checkResult{name, true, "not checked (could not inspect image " + source + "): " + firstLine(err.Error()), ""}
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return checkResult{name, true, "not checked: no " + leverVersionLabel + " label on " + source +
			" — the image predates the label (or was not built by `make lever-image`); rebuild it so this row can compare it with the host lever", ""}
	}
	if release := strings.Fields(v)[0]; release != host {
		return checkResult{name, false,
			fmt.Sprintf("%s carries lever %s but the host lever is %s — its in-jail binaries (lever-agent, lever-manager) were built from another release", source, v, hostFull),
			"rebuild the agent image from this lever's source (`make lever-image LEVER_IMAGE_FORCE=1`, then any instance image built FROM it), " +
				"then `lever apply`; a running manager keeps its image until `lever up --fresh` (the conversation is discarded)"}
	}
	if v == hostFull {
		return checkResult{name, true, fmt.Sprintf("%s carries lever %s, the same as the host", source, v), ""}
	}
	return checkResult{name, true,
		fmt.Sprintf("%s carries lever %s, host lever is %s — same release %s (the release is compared, not the commit)", source, v, hostFull, host), ""}
}

// checkPATTokens judges the hub tokens on disk by the records lever kept at
// mint (state.PATRecord): the scope set must be the one this lever mints
// with, and the expiry must be outside apply's renew window. The hub itself
// cannot be asked — a UAT may not list tokens — so a token without a record
// is a finding: it was minted by a lever that recorded nothing, which also
// means scion's 90-day default expiry and no agent:message. The fix is always
// the same re-apply, which re-mints inside the throwaway dev-auth window.
func checkPATTokens(ctx context.Context, st state.State, remoteEnabled bool, known scopeKnownFunc, now time.Time) checkResult {
	const name = "hub tokens"
	const fix = "run `lever apply` (it re-mints the token in the bootstrap dev-auth window and revokes the old one); " +
		"when agents are running and the scope set changed, apply cannot open that window: run `lever stop`, then `lever up`"
	type tokenCheck struct {
		label string
		load  func() (string, error)
		rec   func() (state.PATRecord, bool, error)
		want  []string
	}
	crec, _, _ := st.LoadControllerPATRecord()
	checks := []tokenCheck{{"controller", st.LoadControllerPAT, st.LoadControllerPATRecord,
		controllerPATScopes(controllerLifecycle(ctx, known, crec))}}
	if remoteEnabled {
		checks = append(checks, tokenCheck{"remote", st.LoadRemotePAT, st.LoadRemotePATRecord, remotePATScopes()})
	}
	var details []string
	for _, c := range checks {
		tok, err := c.load()
		if err != nil {
			return checkResult{name, false, c.label + " token: " + err.Error(), fix}
		}
		rec, found, err := c.rec()
		if err != nil {
			return checkResult{name, false, c.label + " token record: " + err.Error(), fix}
		}
		if reason := patMintReason(tok, rec, found, c.want, now); reason != "" {
			return checkResult{name, false, c.label + " token: " + reason, fix}
		}
		if rec.ExpiresAt.IsZero() {
			details = append(details, c.label+" expiry unknown (scion printed none at mint)")
			continue
		}
		details = append(details, fmt.Sprintf("%s expires in %d days (%s)",
			c.label, int(rec.ExpiresAt.Sub(now).Hours()/24), rec.ExpiresAt.Format("2006-01-02")))
	}
	return checkResult{name, true, strings.Join(details, "; "), ""}
}

// checkRemoteWebRole reports whether the remote web UI's hub users hold the
// lever-remote project role (see remoteWebRoleName) and the project-create
// ceiling (ensureRemoteCeiling): remote-role.json must cover every allowed
// user with the current permission set and a ceiling. It reads that
// record only — the grant needs hub-admin, which doctor never has — so it
// says what lever last granted, not what the hub holds now.
func checkRemoteWebRole(ctx context.Context, st state.State, remote remoteAccess, known scopeKnownFunc) checkResult {
	const name = "remote web role"
	if !remote.Enabled {
		return checkResult{name, true, "disabled", ""}
	}
	const fix = "run `lever apply` (it grants the role in the bootstrap dev-auth window)"
	rec, found, err := st.LoadRemoteRoleRecord()
	if err != nil {
		return checkResult{name, false, err.Error(), fix}
	}
	perms := remoteRolePermissions(remoteRoleLifecycle(ctx, known, rec))
	reason := remoteRoleReason(rec, found, remote.Emails, remote.Contacts, perms)
	if reason == "" {
		var ops []string
		for _, e := range remote.Emails {
			if !slices.Contains(remote.Contacts, e) {
				ops = append(ops, e)
			}
		}
		bound := fmt.Sprintf("%s bound on the project for %s", remoteWebRoleName, strings.Join(ops, ", "))
		if len(ops) == 0 {
			bound = "no operator login"
		}
		if len(remote.Contacts) > 0 {
			bound += fmt.Sprintf("; %s for %s", contactRoleName, strings.Join(remote.Contacts, ", "))
		}
		return checkResult{name, true, bound + "; " + projectCreatePermission + " withheld by an access constraint", ""}
	}
	if strings.HasPrefix(reason, remoteCeilingMissing) {
		return checkResult{name, false, reason, fix}
	}
	detail := "the web UI will answer 403: " + reason
	for _, e := range remote.Emails {
		if _, bound := rec.Bound[e]; !bound && !slices.Contains(rec.Pending, e) {
			return checkResult{name, false, detail, fix}
		}
	}
	if len(rec.Pending) > 0 && hubapi.SamePermissions(rec.Permissions, perms) {
		return checkResult{name, false, detail, remoteRoleFix}
	}
	return checkResult{name, false, detail, fix}
}

// roleCeilingReader reads the project's hub-side role ceiling. Injected so
// the check is unit-testable without a hub.
type roleCeilingReader func(ctx context.Context, project string) (hubapi.RoleCeiling, error)

// checkAgentRoleCeiling reports whether the hub caps agent creates in the
// project at the role lever stamps (apply.Deps.EnsureAgentRoleCeiling). An
// unset ceiling means the hub reads `full`, and lever's own --role stamp is
// then the only bound.
//
// The setting exists only on a scion with roles (both landed in scion#1089),
// so the installed binary is probed first, as checkAgentRoles does: a scion
// that predates roles has no ceiling to read and that is not a finding.
//
// An unreachable hub is not a finding; a hub that ANSWERED unusably is, as for
// shared directories.
func checkAgentRoleCeiling(ctx context.Context, project, want string, rolesSupported func(context.Context) (bool, error), read roleCeilingReader) checkResult {
	const name = "agent role ceiling"
	if read == nil || rolesSupported == nil {
		return checkResult{name, true, "not checked", ""}
	}
	roles, perr := rolesSupported(ctx)
	if perr != nil {
		return checkResult{name, true, fmt.Sprintf("not checked (cannot tell whether this scion understands roles: %v)", perr), ""}
	}
	if !roles {
		return checkResult{name, true, "not applicable: this scion predates agent roles (scion#1089), so it has no ceiling setting", ""}
	}
	c, err := read(ctx, project)
	if err != nil {
		if hubAnswered(err) {
			return checkResult{name, false,
				"could not read the project's settings: " + err.Error(),
				"the hub answered, so this is not a down instance — check the controller PAT " +
					"(`" + stateDirName() + "/`) and that the hub knows a project named " + project}
		}
		return checkResult{name, true, "not checked (hub not reachable): " + err.Error(), ""}
	}
	if c.Max == want && c.Default == want {
		return checkResult{name, true,
			fmt.Sprintf("the hub caps every agent create in %s at %s (max and default)", project, want), ""}
	}
	show := func(v string) string {
		if v == "" {
			return "unset (the hub reads that as full)"
		}
		return v
	}
	return checkResult{name, false,
		fmt.Sprintf("project %s: max agent role %s, default %s; want both %s", project, show(c.Max), show(c.Default), want),
		"run `lever apply` (register-project writes the ceiling through the project settings route)"}
}

// scionTelemetryEnv is the variable scion turns the settings' telemetry.enabled
// into for every agent it starts (pkg/config/telemetry_convert.go).
const scionTelemetryEnv = "SCION_TELEMETRY_ENABLED"

// settingsReader returns the jail's ~/.scion/settings.yaml ("" when absent).
type settingsReader func(ctx context.Context) ([]byte, error)

// envReader reads one variable from a jail container's creation env
// (jail.ContainerEnvValue in production).
type envReader func(ctx context.Context, ref, key string) (value string, set bool, err error)

// readJailScionSettings is the production settingsReader, as the run user
// (the same file the hub and runtime broker read).
func readJailScionSettings(jr proc.Runner) settingsReader {
	return func(ctx context.Context) ([]byte, error) {
		res, err := jr.Run(ctx, nil, "sh", "-c", `f="$HOME/`+layout.SettingsRel+`"; if [ -f "$f" ]; then cat "$f"; fi`)
		if err != nil {
			return nil, err
		}
		return []byte(res.Stdout), nil
	}
}

// checkScionTelemetry reports the agents' sciontool telemetry posture: what
// the jail's settings say (config scion.telemetry, written by apply), and
// whether the running manager was started with it. scion#1792 turned
// telemetry on by default; with no cloud destination every Claude hook waits
// out OTLP timeouts against a receiver that never started, so a trivial turn
// takes minutes while every other row is green. The settings reach an agent
// only at START, so a settings file that is right beside a manager created
// before it is the case this row exists to name.
func checkScionTelemetry(ctx context.Context, mode config.ScionTelemetryMode, read settingsReader, project, name string, list agentLister, env envReader) checkResult {
	const check = "scion telemetry"
	if read == nil {
		return checkResult{check, true, "not checked", ""}
	}
	raw, err := read(ctx)
	if err != nil {
		return checkResult{check, true, "not checked (could not read the jail's scion settings): " + firstLine(err.Error()), ""}
	}
	setting, err := guest.ScionTelemetryEnabled(raw)
	if err != nil {
		return checkResult{check, false, "the jail's " + layout.SettingsRel + " cannot be read as settings: " + firstLine(err.Error()),
			"fix the file in the jail, then run `lever apply`"}
	}
	settingOff := setting == "false"
	describe := "telemetry.enabled: false"
	if !settingOff {
		describe = "telemetry ON (scion's default)"
		if setting == "true" {
			describe = "telemetry.enabled: true"
		}
	}
	if mode == config.ScionTelemetryOff && !settingOff {
		return checkResult{check, false,
			fmt.Sprintf("scion.telemetry is off but the jail's settings say %s — every agent hook waits out OTLP export timeouts", describe),
			"run `lever apply`, then `lever stop && lever up` so the manager starts with it"}
	}
	detail := fmt.Sprintf("scion.telemetry %s; jail settings: %s", mode, describe)
	if !settingOff || list == nil || env == nil {
		// scion-default with scion's own telemetry: the operator's choice, and
		// nothing lever promised to compare the manager against.
		return checkResult{check, true, detail, ""}
	}
	agents, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, detail + "; manager not checked (could not list agents)", ""}
	}
	a := scionpkg.FindAgent(agents, name)
	if a == nil {
		return checkResult{check, true, detail + "; no manager record to compare", ""}
	}
	ref := a.ContainerID
	if ref == "" {
		ref = jail.ContainerName(hubProjectKey(project), name)
	}
	v, set, err := env(ctx, ref, scionTelemetryEnv)
	if errors.Is(err, jail.ErrNoContainer) {
		return checkResult{check, true, detail + "; no manager container to compare", ""}
	}
	if err != nil {
		return checkResult{check, true, detail + "; manager not checked (could not inspect its container)", ""}
	}
	if set && v == "false" {
		return checkResult{check, true, detail + fmt.Sprintf("; manager %q runs with %s=false", name, scionTelemetryEnv), ""}
	}
	got := "unset (scion's default: on)"
	if set {
		got = v
	}
	return checkResult{check, false,
		fmt.Sprintf("the jail's settings turn telemetry off but manager %q was started with %s=%s — its every hook still waits out OTLP export timeouts", name, scionTelemetryEnv, got),
		"run `lever stop && lever up` (the conversation is kept) so the manager starts with the current settings"}
}

// devAuthWindowProbeScript prints "token" when the jail user's scion dev token
// exists and "listening" when something answers HTTP on the throwaway hub's
// port. $1 is the dev-token path relative to $HOME, $2 the port.
const devAuthWindowProbeScript = `out=""
[ -e "$HOME/$1" ] && out="token"
curl -s -o /dev/null -m 3 "http://127.0.0.1:$2/healthz" && out="$out listening"
printf '%s' "$out"`

// checkDevAuthWindow reports a bootstrap dev-auth window that was left open:
// the throwaway dev-auth hub still listening on the jail's
// 127.0.0.1:throwawayHubPort, or scion's dev token still on disk. Either is an
// admin credential surface: the hub answers its dev token with super-admin
// authority, and scion reuses a token file it finds on the next dev-auth
// start. The window closes both itself, on success, failure and SIGINT/
// SIGTERM alike (closeDevAuthWindow); what is left here survived a SIGKILL, a
// crash, or a cleanup that failed.
func checkDevAuthWindow(ctx context.Context, jr proc.Runner) checkResult {
	const check = "dev-auth window"
	res, err := jr.Run(ctx, nil, "sh", "-c", devAuthWindowProbeScript, "_", layout.DevTokenRel, strconv.Itoa(throwawayHubPort))
	if err != nil {
		return checkResult{check, false, "could not probe the jail: " + firstLine(err.Error()), "run `lever doctor` again once the jail is up"}
	}
	out := res.Stdout
	listening, token := strings.Contains(out, "listening"), strings.Contains(out, "token")
	switch {
	case listening:
		return checkResult{check, false,
			fmt.Sprintf("a hub answers on the jail's 127.0.0.1:%d, the port of the bootstrap dev-auth hub; it grants super-admin to its dev token, and agents can reach jail-loopback ports", throwawayHubPort),
			"in the jail, `scion server stop` stops it and `rm ~/" + layout.DevTokenRel + "` removes its token; then `lever apply` starts the live hub"}
	case token:
		return checkResult{check, false,
			"the jail user's ~/" + layout.DevTokenRel + " exists; it is a hub admin credential, and the next dev-auth start reuses it",
			"in the jail, `rm ~/" + layout.DevTokenRel + "`"}
	}
	return checkResult{check, true, "closed (no dev-auth hub, no dev token)", ""}
}
