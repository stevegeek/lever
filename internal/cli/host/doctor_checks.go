package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/imageref"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/jailabi"
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/remoteproxy"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/webpush"
)

// checkResult is one diagnostic outcome. detail is shown in both the pass and
// fail lines; fix is a remediation hint shown only on failure.
type checkResult struct {
	name   string
	ok     bool
	detail string
	fix    string
}

// dialFunc returns nil if something listens on addr.
type dialFunc func(addr string) error

func tcpDial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// doctorProbes is every host-side observation doctor makes that reaches
// outside the process. Checks take it as a value so tests never race on a
// package variable.
type doctorProbes struct {
	dial      dialFunc
	goVersion func() (string, error)
	// nodeToolchain validates node and npm for the scion web-asset build.
	nodeToolchain func() (string, error)
	// claudeVersion reads an image's baked Claude Code version label from the host
	// docker store; claudeVersionTar reads it from the image_tar archive, for a
	// host with no docker.
	claudeVersion    func(imageRef string) (string, error)
	claudeVersionTar func(tarPath, imageRef string) (string, error)
	leverVersion     func(imageRef string) (string, error)
	leverVersionTar  func(tarPath, imageRef string) (string, error)
	// jailABI and jailABITar read the lever_jail_abi label (internal/jailabi).
	jailABI       func(imageRef string) (string, error)
	jailABITar    func(tarPath, imageRef string) (string, error)
	remoteHealthz func(healthzProbe) (int, error)
	// portHolder names the process on a host loopback port ("pid 123, python3"),
	// or "" when it cannot tell.
	portHolder func(ctx context.Context, port int) string
}

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
		jailABI: func(imageRef string) (string, error) { return imageLabelProbe(r, imageRef, jailabi.Label) },
		jailABITar: func(tarPath, imageRef string) (string, error) {
			return jail.ImageTarLabel(tarPath, imageRef, jailabi.Label)
		},
		remoteHealthz: remoteHealthzProbe,
		portHolder:    func(ctx context.Context, port int) string { return lsofHolder(ctx, r, port) },
	}
}

var doctorHTTPClient = &http.Client{Timeout: 3 * time.Second}

// stateRel renders a state-dir file relative to the instance root, as fix
// text names it.
func stateRel(st state.State, path string) string {
	return filepath.Join(filepath.Base(st.Dir), filepath.Base(path))
}

func stateDirName() string { return state.DirName }

// stateInTree is brokerctl.StateInsideTreeErr for the doctor rows: whether
// the state directory counts as inside the tree, what the row says about it,
// and the fix. An identity check that failed counts as inside, and the row
// names its error.
func stateInTree(app *config.App, st state.State) (inside bool, what, fix string) {
	inside, err := brokerctl.StateInsideTreeErr(app, st)
	if err != nil {
		return true, "lever " + err.Error() + ", so it counts as inside the tree",
			"make " + st.Dir + ", " + app.Tree + " and the directories above them searchable by you"
	}
	return inside, "the state directory " + st.Dir + " is inside the tree " + app.Tree,
		"point `tree:` at a subdirectory that does not contain " + stateDirName() + "/"
}

// checkListeningProcess is the pid-then-port ladder shared by the broker and
// remote-proxy checks. It tells apart never started, died (stale pid), and
// alive but not serving.
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

func checkBrokerAlive(st state.State, jailPort int, p doctorProbes) checkResult {
	return checkListeningProcess("broker running", "broker.pid", "the broker", stateRel(st, st.Log()),
		"run `lever apply` or `lever up`", func() (int, bool, bool) { return state.PIDStatus(st.PID()) }, fmt.Sprintf("127.0.0.1:%d", jailPort), p.dial)
}

type healthzProbe struct {
	// Addr is config.App.RemoteProbeAddr.
	Addr string
	// Port is the proxy's port. Host is always 127.0.0.1:<Port>, whatever Addr
	// is: the name the proxy's Host gate admits for host-side probes
	// (remoteproxy.hostAllowed).
	Port int
	// Login, when non-empty, is sent in Header.
	Header, Login string
}

// remoteHealthzProbe issues GET /healthz against the local proxy. Doctor runs
// host-side, as trusted as the remote.pat it reads, so it sends the first
// allowed user in the identity header rather than let a pinned instance 403
// its own probe; an unpinned instance sends no header.
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

// checkRemote verifies the remote-access proxy when configured: the process is
// alive and listening on EffectiveRemotePort, the remote PAT apply mints is not
// group/other-accessible (a world-readable token is a leak even unsent), and
// GET /healthz through the proxy returns 200. The proxy opens a hub web
// session with a lever assertion before forwarding, so a 200 proves the
// sign-in; it signs in an invited login (allowed_users[0], or the unnamed
// operator), never creates one.
//
// Disabled is a pass: remote access is opt-in. PAT expiry is not checked:
// lever has no cheap read for it.
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
	status, err := p.remoteHealthz(healthzProbe{Addr: addr, Port: port,
		Header: app.EffectiveRemoteIdentityHeader(), Login: firstOrEmpty(app.Remote.Logins())})
	if err != nil {
		return checkResult{name, false, fmt.Sprintf("GET /healthz through the proxy failed: %v", err), "inspect " + remoteLog + " — the hub may be down, or the proxy misconfigured"}
	}
	if status != http.StatusOK {
		return checkResult{name, false, fmt.Sprintf("GET /healthz through the proxy returned %d, want 200", status), "inspect " + remoteLog + " and " + stateRel(st, st.RemoteAudit())}
	}
	return checkResult{name, true, alive.detail + ", PAT present, healthz OK (the proxy signed in with a lever assertion)", ""}
}

// checkRemoteExposure warns per remote setting that weakens a default
// protection (config.App.RemoteWarnings). lever cannot see the host firewall
// these rest on, so the row restates what the operator must keep true.
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
// chatledger): the proxy records only verified logins, and the ledger proves
// something only while no jail can write it.
func checkVerifiedChat(app *config.App, st state.State) checkResult {
	const name = "verified chat"
	if !app.RemoteEnabled() {
		return checkResult{name, true, "remote access disabled (no web chat)", ""}
	}
	if inside, what, fix := stateInTree(app, st); inside {
		return checkResult{name, false, "off: " + what + " (agents mount the tree), so a ledger there proves nothing", fix}
	}
	if len(app.Remote.AllowedUsers) == 0 {
		return warnResult(name, "off: remote.allowed_users is empty, so the proxy verifies no login and records no chat; "+
			"agents cannot verify a web chat post and treat every one as data (they do not act on it or reply)",
			"list the operator's login in remote.allowed_users, then run `lever apply` and `lever init`")
	}
	p := brokerctl.ChatLedgerPath(app, st)
	// Every allowed login speaks with operator authority once verified: name them.
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

// agentMsgsLive is what the running broker and remote proxy were started with.
// brokerHash's ok is false when no broker answers.
type agentMsgsLive struct {
	brokerHash   func() (string, bool)
	proxyMatches func(hash string) (running, matches bool)
	// scionHasCommit reports whether the scion.source checkout holds commit.
	scionHasCommit func(source, commit string) (bool, error)
}

func liveAgentMessages(ctx context.Context, app *config.App, st state.State) agentMsgsLive {
	return agentMsgsLive{
		brokerHash: func() (string, bool) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			// A redirect is not the broker's answer: never followed.
			client := &http.Client{Timeout: 2 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			er, err := readBrokerIdentity(ctx, client, fmt.Sprintf("http://127.0.0.1:%d", app.EffectiveAdminPort()))
			if err != nil {
				return "", false
			}
			// Another instance's broker says nothing about this one's.
			if ours, err := st.ReadInstanceID(); err != nil || er.InstanceID != ours {
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
		scionHasCommit: func(source, commit string) (bool, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return scionbin.SourceHasCommit(ctx, proc.RealRunner{}, source, commit)
		},
	}
}

// checkAgentMessages reports remote.agent_messages. The broker and proxy read
// the config only at start, so while on, the row says when either runs with
// another config or version; while off, only when a process runs with this
// config plus agent messages on.
func checkAgentMessages(app *config.App, st state.State, live agentMsgsLive) checkResult {
	r := agentMessagesRow(app, st)
	if !r.ok {
		return r
	}
	on := app.AgentMessagesOn()
	var scionNote string
	if on {
		bad, note := agentMessagesScion(app, live.scionHasCommit)
		if bad {
			return checkResult{r.name, false, "on, but " + note,
				"point scion.source/version at " + config.AgentMessagesScionFloor[:8] + " or later, or turn remote.agent_messages off"}
		}
		if note != "" {
			scionNote = "; " + note
		}
	}
	onApp := *app
	onApp.Remote.AgentMessages.Enabled = true
	var stale []string
	// The broker probe (up to 2 s) runs only when the setting is or could be on.
	if live.brokerHash != nil && (on || onApp.AgentMessagesOn()) {
		if h, ok := live.brokerHash(); ok && (on && h != brokerctl.ConfigHash(app) || !on && onApp.AgentMessagesOn() && h == brokerctl.ConfigHash(&onApp)) {
			stale = append(stale, "broker")
		}
	}
	if live.proxyMatches != nil {
		if on {
			if running, ok := live.proxyMatches(brokerctl.RemoteConfigHash(app, st)); running && !ok {
				stale = append(stale, "remote proxy")
			}
		} else if onApp.AgentMessagesOn() {
			if running, ok := live.proxyMatches(brokerctl.RemoteConfigHash(&onApp, st)); running && ok {
				stale = append(stale, "remote proxy")
			}
		}
	}
	if len(stale) > 0 {
		return warnResult(r.name, r.detail+"; the running "+strings.Join(stale, " and ")+
			" started with another config; agent message settings take effect after `lever init` + `lever apply`"+scionNote,
			"run `lever init`, then `lever apply`")
	}
	if scionNote != "" {
		return warnResult(r.name, r.detail+scionNote,
			"use a scion at "+config.AgentMessagesScionFloor[:8]+" or later (a scion.source checkout or a pseudo-version lever can check)")
	}
	return r
}

// agentMessagesScion checks the configured scion against
// config.AgentMessagesScionFloor, the first hub that sets a message's sender
// itself (the filter trusts it). bad: known older, or a checkout or shallow
// clone without the floor commit. note is set when lever cannot tell.
func agentMessagesScion(app *config.App, hasCommit func(source, commit string) (bool, error)) (bad bool, note string) {
	floor := config.AgentMessagesScionFloor[:8]
	sc := app.Scion
	switch {
	case sc.Version != "":
		if app.ScionVersionPredatesAgentMessages() {
			return true, "scion.version " + sc.Version + " predates " + floor + ": its hub takes a message's sender from the caller, so an agent can post as its contact"
		}
		if _, ok := config.ScionVersionTime(sc.Version); !ok {
			return false, "cannot tell whether scion.version " + sc.Version + " holds " + floor + " (the hub sets a message's sender from " + floor + " on)"
		}
	case sc.Source != "":
		if hasCommit == nil {
			return false, ""
		}
		has, err := hasCommit(sc.Source, config.AgentMessagesScionFloor)
		if errors.Is(err, scionbin.ErrShallowSource) {
			return true, "the scion.source checkout is a shallow clone that does not show " + floor + ", so it cannot be shown to set a message's sender itself (run git fetch --unshallow)"
		}
		if err != nil {
			return false, "cannot tell whether the scion.source checkout holds " + floor + ": " + err.Error()
		}
		if !has {
			return true, "the scion.source checkout does not hold " + floor + " (it predates it, or is a fork without it): its hub takes a message's sender from the caller, so an agent can post as its contact"
		}
	case sc.Binary != "":
		return false, "cannot tell whether the scion.binary holds " + floor + " (the hub sets a message's sender from " + floor + " on)"
	}
	return false, ""
}

func agentMessagesRow(app *config.App, st state.State) checkResult {
	const name = "agent messages"
	if !app.AgentMessagesOn() {
		return checkResult{name, true, "off (agents answer contacts as before; nothing is filtered)", ""}
	}
	if inside, what, fix := stateInTree(app, st); inside {
		return checkResult{name, false, "on, but " + what + ": no record can be kept, so contacts see no agent message", fix}
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
	// The hourly rate counts over all contact files, so one unsafe file refuses
	// every authorization (fail closed).
	if bad := unsafeLedgerFile(p); bad != "" {
		return checkResult{name, false, "the agent ledger file " + stateRel(st, bad) + " is not a private regular file of yours: " +
			"every agent's contact_message is refused (unavailable) and contacts are shown no agent message until it is fixed",
			"chmod 600 " + bad + " (or remove it, which forgets that contact's records)"}
	}
	return checkResult{name, true, detail + "; ledger " + stateRel(st, p) + "/ (0700)", ""}
}

// unsafeLedgerFile is the first contact file in dir (or its .1) that is not a
// regular file, is writable by others, or belongs to another user; "" if none.
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

func shortDuration(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}

// checkChatLabels reads remote.labels_file as the proxy does. A bad file only
// warns: it is agent-written and only costs the chat page its labels.
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

// labelFault names a labels-file fault in fixed words.
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

// checkPush reports remote.push. The proxy writes push/status.json at start and
// after each send, so the row shows the last outcome.
func checkPush(app *config.App, st state.State) checkResult {
	const name = "push"
	if !app.PushOn() {
		return checkResult{name, true, "off (no service worker, no push routes, no hub streams)", ""}
	}
	if inside, what, fix := stateInTree(app, st); inside {
		return checkResult{name, false, "on, but " + what + ": push stays off (agents could read its key)", fix}
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

// warnResult is a passing checkResult that carries a fix, which
// printDoctorReport renders with "!". Doctor's exit status ignores it.
func warnResult(name, detail, fix string) checkResult {
	return checkResult{name, true, detail, fix}
}

func firstOrEmpty(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// checkToolBackends verifies every broker tool up front: an external tool must
// listen on its loopback backend, and a supervised tool's command must resolve
// on the supervisor PATH (it would otherwise fail silently at spawn). dir is
// the instance dir, where the supervisor runs every tool, so a relative
// command resolves against it.
func checkToolBackends(tools []config.Tool, dir string, p doctorProbes) checkResult {
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
			if p, err := config.ResolveToolCommand(bin, dir); err != nil {
				down = append(down, fmt.Sprintf("%s (supervised, %q not on PATH)", t.Name, bin))
			} else if !config.IsExecutableFile(p) {
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

// checkScionProject flags bad-teardown corruption: scion has registered the
// tree but the in-tree marker is gone, or the tree is registered twice. Either
// makes `scion init` fail with "existing project marker is invalid", blocking
// the manager.
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

type sharedDirLister func(ctx context.Context, project string) ([]hubapi.SharedDir, error)

// hubAnswered reports whether the hub replied with something lever could not
// use, as opposed to never being reached.
func hubAnswered(err error) bool {
	var apiErr *hubapi.APIError
	return errors.As(err, &apiErr)
}

// checkProjectSharedDirs reports any directory the hub mounts into every agent
// of the project. scion stamps a writable `scratchpad` on new projects, a
// read/write channel between the manager and every worker that defeats subtree
// isolation, so apply strips it; an entry here means the strip did not run or
// something re-added one.
//
// An unreachable hub is not a finding (the broker check covers a stopped
// instance), but anything the hub answered unusably (401, 403, an unlisted
// project) fails; the detail says which. An agent started before the removal
// keeps its bind mount until it restarts, so a pass speaks for new agents.
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

// agentListSizer returns the size in bytes of the largest page of the
// project's agent list.
type agentListSizer func(ctx context.Context, project string) (int, error)

// checkAgentListSize warns while the hub's agent list still fits the reply
// cap lever reads it under: past the cap, resume and the stored-role check
// refuse to run. Each record carries its full task text, so a few very long
// tasks can fill a page.
func checkAgentListSize(ctx context.Context, project string, measure agentListSizer) checkResult {
	const name = "hub agent list"
	capMiB := hubapi.AgentListReplyCap >> 20
	if measure == nil {
		return checkResult{name, true, "not checked", ""}
	}
	largest, err := measure(ctx, project)
	if err != nil {
		var apiErr *hubapi.APIError
		if errors.As(err, &apiErr) && apiErr.TooLarge {
			return checkResult{name, false, err.Error(),
				"a page of agent records passed what lever reads, so `lever up` cannot check or resume its agents; " +
					"delete agents you no longer need (each record carries its full task text)"}
		}
		// The roles row reports a hub that answered unusably.
		return checkResult{name, true, "not checked: " + err.Error(), ""}
	}
	detail := fmt.Sprintf("largest page %d KiB of the %d MiB lever reads", largest>>10, capMiB)
	if largest > hubapi.AgentListReplyCap/2 {
		return checkResult{name, true, detail,
			"agent records carry their full task text; delete agents you no longer need before a page reaches the cap, " +
				"when `lever up` can no longer check or resume them"}
	}
	return checkResult{name, true, detail, ""}
}

type agentRoleLister func(ctx context.Context, project string) ([]hubapi.Agent, error)

// checkAgentRoles reports agent records that store no authorization role.
//
// The role is written at create and immutable after, and scion resolves an
// unset stored role to full at dispatch and on every token refresh. `scion
// resume` takes no --role, so the only repair is deleting the agent.
//
// On a roles-aware scion an unrolled record is a live promotion and fails. On
// an older one it is harmless, but the row warns, since it is where an
// operator learns before a pin bump that the bump will promote them. An
// unreachable hub is not a finding; one that answered unusably is.
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

	// A grandfathered role is scion's migration writing `full` where nothing was
	// stored: the same hazard as an unrolled record.
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

	// The scion is probed only now, so one that cannot answer does not turn a
	// clean instance into a finding.
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

// agentSessionReader reads an agent's hub token expiry and whether its harness
// runs, from inside its container. Both are the agent's own word: doctor turns
// them into a verdict and a time and never prints what the container said.
type agentSessionReader interface {
	HubToken(ctx context.Context, ref string) (jail.HubTokenTimes, error)
	HarnessAlive(ctx context.Context, ref string) (bool, error)
}

// agentLister reads scion's agent records: phase and container status, which
// the hub's record (hubapi.Agent) lacks.
type agentLister func(ctx context.Context, project string) ([]scionpkg.Agent, error)

// checkManagerLive reports whether the manager is actually up: a record, a
// running phase, a live container, and a harness still turning. It is the one
// row that says the agent works, not just the plumbing.
//
// Activity is what scion's Claude Code hooks report (scion.Activity*), with
// its age. Crashed and offline fail. Stalled passes: the hub's stall sweeper
// marks an idle manager the same way as a stuck one, so the row names both
// readings (the guest DNS row fails on the common cause). A list error is "not
// checked", never a pass.
//
// The session reader (nil = not probed) exposes two faults behind a live
// container. An expired hub token fails the row whatever the activity:
// sciontool's refresh timer stands still while the host sleeps and an expired
// token cannot refresh itself. A stopped phase over a running container is
// told apart by whether claude still runs (another claude firing the shared
// SessionEnd hook leaves it running).
//
// A forced resume that died because claude --continue found no conversation
// is named as such: logTail (nil = not read) is matched for a fixed phrase only
// (apply.NoConversationInLog) and never printed.
func checkManagerLive(ctx context.Context, project, name string, list agentLister, session agentSessionReader, logTail containerLogReader, now time.Time) checkResult {
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
			// The stall sweeper also marks a manager idle at its prompt as stalled, the
			// normal state of an instance nobody talked to, so it cannot fail the row. The
			// detail says how to tell a stuck turn apart.
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
		// The container column comes from the runtime broker's heartbeat; a blank is
		// "cannot tell" (as for apply.ObserveManagerLive), never a death or a pass.
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
	detail := fmt.Sprintf("manager %q is not live: phase %s, container %s", name, scionpkg.BoundedQuote(a.Phase), scionpkg.BoundedQuote(a.ContainerStatus))
	// Only an error-phase record: `lever up` resumes it with --force and
	// --continue. A stopped record resumes in a new session anyway.
	if a.Phase == scionpkg.PhaseError && logTail != nil {
		if tail, err := logTail(ctx, ref); (err == nil || tail != "") && apply.NoConversationInLog(tail) {
			return checkResult{check, false, detail + " — its claude found no conversation to continue, so the manager was most likely never used", noConversationFix}
		}
	}
	return checkResult{check, false, detail, fix}
}

// containerLogReader reads the tail of an agent container's log: the agent's
// word, matched for fixed phrases and never printed.
type containerLogReader func(ctx context.Context, ref string) (string, error)

// noConversationFix is the fix for a manager whose claude found no
// conversation to continue.
const noConversationFix = "run `lever up`: it starts the manager fresh on its own once it has checked from the guest that the agent home holds no transcript. " +
	"`lever up --fresh` starts it fresh in any case, and loses nothing: it backs up any transcript first"

// stoppedResumeFix: scion's hub resumes a stopped record with a fresh harness
// session (only a suspended one gets --continue).
const stoppedResumeFix = "run `lever up` — scion resumes a stopped record in a new claude session (no --continue); the old conversation stays in the agent home: `lever attach`, then `/resume`"

func hubTokenFix(name, project string) string {
	return "run `lever apply` — it runs `scion reset-auth` for each running agent whose hub token expired (no restart; the conversation is kept). By hand, in the guest: `scion reset-auth " + name + " -g " + project + "` with the controller PAT"
}

// checkAgentHubTokens fails on an expired hub token of the manager or any live
// configured worker: everything that agent sends fails with 401 while its other
// rows stay green. A token past its refresh point (2 h before expiry) warns:
// sciontool's refresh timer counts the guest's monotonic clock, which stands
// still while the host sleeps. An unreadable token is "not checked".
//
// Only a running or stopped agent fails, the phases `lever apply` heals; under
// other phases the needed resume issues a new token. A running manager's
// expired token is already the manager row's failure, so here it only warns.
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
			unread = append(unread, name+" ("+jail.ProbeErrorClass(err)+")")
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
		notChecked = "; not checked (token unreadable): " + strings.Join(unread, ", ")
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

// guestDNSProbeName is the name every subscription-mode agent must resolve on
// its first turn.
const guestDNSProbeName = "api.anthropic.com"

// checkGuestDNS resolves a public name from inside the guest, through the
// agents' resolver path: a guest without DNS otherwise looks healthy and idle
// while every LLM call times out. Closed egress drops DNS by design, so the row
// is informational there. The probe is bounded because a dead resolver makes
// getent wait on systemd-resolved's retries.
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

// legacyKVMDropInTest is true while a legacy podman drop-in that put /dev/kvm
// in every container is still in the guest.
const legacyKVMDropInTest = `test -e "$HOME/.config/containers/containers.conf.d/20-lever-kvm.conf"`

// checkNestedVirt reports whether nested_virt reached the jail and only the
// manager: on, /dev/kvm in the guest with mode 0666 and in the manager's
// container (only the exit code of `test -c` is read); on or off, no legacy
// drop-in giving every container the device. Off, a Lima manager created while
// it was on still holds the device. Not applicable off Lima.
func checkNestedVirt(ctx context.Context, on, lima bool, jr proc.Runner, managerRef string, probe func(context.Context, string, string) (bool, error)) checkResult {
	const check = "nested virt"
	if !on && !lima {
		return checkResult{check, true, "not applicable (nested_virt off)", ""}
	}
	if on {
		res, err := jr.Run(ctx, nil, "stat", "-c", "%F %a", "/dev/kvm")
		got := strings.TrimSpace(res.Stdout)
		if err != nil || got != "character special file 666" {
			if got == "" {
				got = "missing"
			}
			return checkResult{check, false, "guest /dev/kvm: " + got,
				"`lever apply` installs the rule; if /dev/kvm is missing the VM was created before nested_virt or the host nested module is off: `lever conversation backup`, `lever destroy`, `lever up`, `lever conversation restore`"}
		}
	}
	if res, err := jr.Run(ctx, nil, "sh", "-c", legacyKVMDropInTest); err == nil && res.Code == 0 {
		return checkResult{check, false, "the legacy podman drop-in that gives every container in the guest /dev/kvm (workers too) is present",
			"run `lever apply` (removes it); workers created while it was there keep /dev/kvm until they are recreated"}
	}
	if probe == nil || managerRef == "" {
		if !on {
			return checkResult{check, true, "nested_virt off; manager container not checked", ""}
		}
		return checkResult{check, true, "guest ready; manager container not checked", ""}
	}
	has, err := probe(ctx, managerRef, apply.KVMDevice)
	if err != nil {
		if !on {
			return checkResult{check, true, "nested_virt off; manager container not checked: " + firstLine(err.Error()), ""}
		}
		return checkResult{check, true, "guest ready; manager container not checked: " + firstLine(err.Error()), ""}
	}
	switch {
	case !on && has:
		return checkResult{check, false, "nested_virt is off but the manager still has /dev/kvm (created while it was on)", apply.NestedVirtRecreateFix}
	case !on:
		return checkResult{check, true, "nested_virt off; the manager has no /dev/kvm", ""}
	case !has:
		return checkResult{check, false, "the manager container has no /dev/kvm (created before nested_virt)", apply.NestedVirtRecreateFix}
	}
	return checkResult{check, true, "guest /dev/kvm 0666, manager has /dev/kvm, no legacy drop-in", ""}
}

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
// against manager.image, under the name agents run it as (imageref.Agent). A
// record keeps its image for life, so after an edit the version row (which
// inspects the configured image) reads green while the manager runs something
// else. Podman may qualify the ref (docker.io/, localhost/), so refs are
// compared normalised.
//
// A record created on an unqualified name, by an older lever, is a warning:
// scion rewrites that name to the hub's image registry at every start, and
// any agent can set the registry between applies.
func checkManagerImage(ctx context.Context, project, name, configured string, list agentLister) checkResult {
	const check = "manager image"
	if list == nil || configured == "" {
		return checkResult{check, true, "not checked", ""}
	}
	want := imageref.Agent(configured)
	agents, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var bare []string
	for _, ag := range agents {
		if ag.Image != "" && !strings.Contains(ag.Image, "@") && !imageref.Qualified(ag.Image) {
			bare = append(bare, fmt.Sprintf("%s (%s)", ag.Slug, ag.Image))
		}
	}
	bareDetail := ""
	if len(bare) > 0 {
		bareDetail = fmt.Sprintf("%s created on an unqualified image name, which scion rewrites to the hub's image registry at every start; an agent can point that registry at its own between applies", strings.Join(bare, ", "))
	}
	const bareFix = "recreate them on " + imageref.Registry + "/ names: the manager with `lever up --fresh --keep-conversation`, a worker by recycling or deleting it from the manager"
	a := scionpkg.FindAgent(agents, name)
	switch {
	case a != nil && a.Image != "" && !jail.SameImageRef(a.Image, want) && !jail.SameImageRef(a.Image, configured):
		detail := fmt.Sprintf("manager %q was created on %s but manager.image is now %s — a resume keeps the record's image", name, a.Image, want)
		if bareDetail != "" {
			detail += "; " + bareDetail
		}
		return checkResult{check, false, detail,
			"run `lever up --fresh --keep-conversation` to recreate the manager on the configured image and keep its conversation (plain `--fresh` starts a new one; both back the old one up first)"}
	case bareDetail != "":
		return warnResult(check, bareDetail, bareFix)
	case a == nil:
		return checkResult{check, true, "not checked (no manager record)", ""}
	case a.Image == "":
		return checkResult{check, true, "not checked (the record reports no image)", ""}
	}
	return checkResult{check, true, fmt.Sprintf("%q runs %s, as configured", name, a.Image), ""}
}

// networkCheckedAgents is the manager (whose scion slug is the instance name)
// and each declared worker.
func networkCheckedAgents(manager string, workers []string) []string {
	return append([]string{manager}, workers...)
}

type netModeLister func(ctx context.Context, ref string) (string, error)

// checkAgentNetwork finds agent containers not in their own pasta netns. On
// podman 4.9 a container created without lever's drop-in runs slirp4netns,
// where host.containers.internal resolves to the guest's own address, which
// LEVER_EGRESS drops, so the agent cannot reach the hub. The mode is fixed at
// create; a stop and an up recreate the container and keep the conversation.
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

type mountLister func(ctx context.Context, ref string) ([]string, error)

// workerTicketMount is the worker's read-only enrolment ticket directory (see
// broker.WorkerSpec.TicketDir).
const workerTicketMount = "/run/lever"

// checkWorkerTicketMounts finds worker records without the /run/lever mount:
// scion keeps a record's volumes for life, so such a worker's next re-enrolment
// cannot find a ticket (the broker stages none in the tree). The fix is a
// purge and a re-dispatch. The container is found by the id scion reports or
// else by scion's container name. A listing or inspect failure is "not
// checked", never a pass.
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

type recordVolumeReader func(ctx context.Context, project, agent string) ([]jail.Mount, error)

type mountInspector func(ctx context.Context, ref string) ([]jail.Mount, error)

// checkManagerReadOnly verifies that the manager container holds every mount
// of its manager.read_only plan (apply.ManagerTreeMountGaps): each entry
// read-only in place; each pin (an entry's ancestors, every worker dir and its
// ancestors) a mount point, which cannot be renamed away; each from the same
// directory of the in-jail tree. scion keeps a record's volumes for life, so
// only a fresh create adds a new entry's mount.
//
// The inspect is not enough: when the host replaces a protected directory
// after the create, podman still lists the read-only mount while container
// writes land in the new directory. So a running container's plan is also
// checked live from the guest side (apply.LiveTreeMountGaps: the kernel's
// mountinfo and each mount's device and inode, never a program inside the
// container, which the agent controls). A check that cannot run, or a stopped
// container, warns, never passes. A first process in a namespace made inside
// the container fails the row.
//
// The row also reports stale mounts (apply.ManagerStaleTreeMounts): one the
// config dropped, or whose host directory is gone, which fails because the
// next resume cannot recreate the container. That half runs even with
// read_only unset, and with no container it reads the hub record's volumes
// (record; nil skips it).
func checkManagerReadOnly(ctx context.Context, project, tree, name string, want []config.TreeMount, list agentLister, inspect mountInspector, record recordVolumeReader, live liveMountChecker) checkResult {
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
	r := managerReadOnlyGaps(ctx, check, project, name, ref, a.ContainerStatus, want, mounts, live)
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

// managerReadOnlyGaps is checkManagerReadOnly's half for the current plan.
func managerReadOnlyGaps(ctx context.Context, check, project, name, ref, containerStatus string, want []config.TreeMount, mounts []jail.Mount, live liveMountChecker) checkResult {
	gaps := apply.ManagerTreeMountGaps(project, want, mounts)
	if gaps.Empty() {
		if !scionpkg.ContainerLive(containerStatus) {
			return warnResult(check, "the mounts are listed, but the live check did not run (the manager container is not running), so a protected directory replaced on the host would go unnoticed",
				"bring the manager up (`lever up`) and re-run doctor")
		}
		if live == nil {
			// A warning row needs a fix to print as "!", never as a pass.
			return warnResult(check, "the mounts are listed, but the live check is not wired, so a protected directory replaced on the host would go unnoticed",
				"this is a lever wiring gap; report it")
		}
		lg, err := apply.LiveTreeMountGaps(ctx, apply.LiveMountChecker(live), project, ref, want)
		switch {
		case errors.Is(err, jail.ErrForeignNamespace):
			return checkResult{check, false, fmt.Sprintf("manager %q: %v", name, err),
				"something in the container moved its first process there to show lever a staged view of its mounts: treat the manager as compromised; back up what you need, then recreate it (`lever up --fresh`)"}
		case err != nil:
			return warnResult(check, "the mounts are listed, but the live check could not run ("+firstLine(err.Error())+"), so protection is not confirmed",
				"re-run doctor once the manager container is up")
		}
		gaps = lg
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
	return checkResult{check, true, fmt.Sprintf("%d path(s) read-only in %q, %d pin(s) mounted (checked live from the guest: each a mount point over the host's directory)", entries, name, pins), ""}
}

// staleTreeMountsResult: a gone directory fails (the next resume fails), a
// dropped mount warns.
func staleTreeMountsResult(check, name string, stale apply.StaleTreeMounts) checkResult {
	detail := fmt.Sprintf("manager %q: %s", name, stale)
	if len(stale.Gone) > 0 {
		return checkResult{check, false, detail, stale.Fix()}
	}
	return warnResult(check, detail, stale.Fix())
}

// checkWorkerTreeBootstraps finds a bootstrap.json under a worker's own tree.
// lever stages a worker's ticket only in the guest runtime directory, mounted
// read-only at /run/lever in that worker alone; a copy in the tree was left
// behind or written by an agent, is readable by the manager, and can mislead a
// worker-side `lever-manager` that reads the wrong path. dirs maps a worker to
// its host directory.
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
		// The tree is agent-writable. A path through a symlink names a file elsewhere,
		// so no `rm` line is printed for it: pasting one could delete the link's
		// target, which can be the manager's own bootstrap.
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

// underRealTree is where p would be if nothing below the tree were a symlink.
// The tree root is the mount point and not an agent's to replace; everything
// below it is agent-writable. A tree behind a host link (/tmp on macOS) does
// not count.
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

// checkRecyclableWorkers names the workers the manager may discard and start
// fresh (`lever-manager agent recycle`). Informational: it shows a power the
// config hands the manager.
func checkRecyclableWorkers(workers []config.Worker) checkResult {
	const check = "worker recycle"
	if len(workers) == 0 {
		return checkResult{check, true, "no workers declared", ""}
	}
	var names []string
	for _, w := range workers {
		if w.Recyclable {
			names = append(names, w.Name)
		}
	}
	if len(names) == 0 {
		return checkResult{check, true, "no worker is recyclable (only `lever worker purge` discards a worker record)", ""}
	}
	slices.Sort(names)
	return checkResult{check, true, fmt.Sprintf("worker %s recyclable: the manager may discard the record (its conversation, never its workspace) and start it fresh with `lever-manager agent recycle`", braceList(names)), ""}
}

func braceList(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return "{" + strings.Join(names, ",") + "}"
}

// backendHostPort reduces "[scheme://]host:port[/path]" to host:port. A
// supervised tool's backend may carry a scheme.
func backendHostPort(backend string) string {
	if i := strings.Index(backend, "://"); i >= 0 {
		backend = backend[i+3:]
	}
	if i := strings.IndexByte(backend, '/'); i >= 0 {
		return backend[:i]
	}
	return backend
}

// checkCredentialFile verifies the subscription credential is present,
// non-empty and not group/other-accessible. The detail reports size and mode
// only, never contents. An unset path passes (api-key instances).
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

// checkMcpJsonInTree flags any .mcp.json under the host tree: Claude loads it
// as project scope in every jailed agent, where its endpoints collide with the
// brokered user-scope tools lever-agent registers. Unreadable directories are
// skipped.
func checkMcpJsonInTree(tree string) checkResult {
	const name = "no stray .mcp.json in tree"
	var found []string
	_ = filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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

// goVersionProbe tells "not on PATH" from "on PATH but broken" (a dead
// asdf/mise shim, typically exit status 126).
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

// checkGoToolchain verifies a working Go toolchain when scion must be built
// from source or a module version. A broken shim otherwise fails with an
// opaque "exit status 126" deep inside apply. With prebuilt, a missing Go
// passes: only a failed download falls back to the build.
func checkGoToolchain(scion config.ScionConfig, prebuilt bool, p doctorProbes) checkResult {
	const name = "go toolchain"
	if scion.Source == "" && scion.Version == "" {
		return checkResult{name, true, "scion build not required", ""}
	}
	out, err := p.goVersion()
	if err != nil && prebuilt {
		return checkResult{name, true, "not required: lever downloads this release's prebuilt scion " + scion.Version +
			" (only a failed download builds it, which needs Go)", ""}
	}
	if err != nil {
		return checkResult{name, false, "go toolchain not usable: " + err.Error(),
			`put a REAL Go toolchain on PATH (not just an asdf/mise shim), e.g. export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"; ` + "`go version` should print"}
	}
	return checkResult{name, true, strings.TrimSpace(out), ""}
}

// scionPrebuilt reports whether apply downloads the release's prebuilt scion
// instead of building it, and its web UI too when the instance serves one (a
// web UI build needs the module source, which is fetched with Go). The guest
// is assumed to run the host's architecture.
func scionPrebuilt(app *config.App) bool {
	return app.Scion.Version != "" && scionbin.DownloadsRelease(app.Scion.Version, runtime.GOARCH) &&
		(!app.ScionWebAssets() || scionbin.DownloadsWebUI(app.Scion.Version))
}

// webUIPrebuilt reports whether apply downloads the release's prebuilt scion
// web UI instead of building it with node.
func webUIPrebuilt(app *config.App) bool {
	return app.ScionWebAssets() && app.Scion.Version != "" && scionbin.DownloadsWebUI(app.Scion.Version)
}

// nodeToolchainProbe validates node and npm for the scion web-asset build. It
// probes inside the build's cache directory (creating it): a version manager
// that walks up for a project file (asdf, mise) answers differently elsewhere.
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

// checkNodeToolchain verifies node and npm can build scion's web UI when the
// instance serves it; otherwise a broken toolchain shows up deep in a failed
// apply. No UI to build passes, and so does a missing node when prebuilt
// (only a failed download builds it).
func checkNodeToolchain(app *config.App, prebuilt bool, p doctorProbes) checkResult {
	const name = "node toolchain"
	if !app.ScionWebAssets() {
		return checkResult{name, true, "scion web UI build not required", ""}
	}
	version, err := p.nodeToolchain()
	if err != nil && prebuilt {
		return checkResult{name, true, "not required: lever downloads this release's prebuilt scion web UI for " + app.Scion.Version +
			" (only a failed download builds it, which needs node and Go)", ""}
	}
	if err != nil {
		fix := ""
		if errors.Is(err, webassets.ErrNodeToolchain) {
			fix = webassets.NodeToolchainFix
		}
		return checkResult{name, false, err.Error(), fix}
	}
	return checkResult{name, true, "node " + strings.TrimSpace(version), ""}
}

// checkOperatorSkills verifies the framework skills from `lever init` are
// present, current and unmodified (or adopted via `lever init --adopt`) and
// referenced from the tree-root CLAUDE.md, in read-only check mode. The
// scaffolds live in the agent-writable tree, so drift past an adopted baseline
// is the tamper signal and is called out separately.
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
		// An adoption pins the file to the baseline it was adopted at, so an instance
		// could upgrade past a security-relevant skill change while doctor reports
		// healthy. Version drift on an adopted file therefore fails: visibility only,
		// never an auto-overwrite.
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

// checkDirectives verifies the operator-directive channel when configured
// (operator.allowed_signers; unset passes). Three things break it without a
// config error: no usable allowed_signers key, ssh-keygen missing from PATH
// (opsig uses it), and, with the broker up, a missing directive socket.
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

// countKeyLines counts "principal keytype keydata" lines, ignoring blanks and
// #-comments. It is only an "at least one key" signal; ssh-keygen is the source
// of truth when verifying.
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

// certRejectWindow is how recent a broker rejection of an expired leaf must be
// to count as active: wider than the agent's handshake-retry cadence, narrow
// enough that the row clears once a re-enrol heals the leaf.
const certRejectWindow = 15 * time.Minute

// brokerLogTailBytes caps how much of broker.out.log is read, from the end.
const brokerLogTailBytes = 64 << 10

// checkAgentCert reports whether the broker is currently rejecting an agent's
// mTLS leaf as expired. A leaf that lapsed while the instance was down shows
// only as a TLS handshake error in the broker's log (the CA stays valid), so
// this scans the log rather than any cert file.
func checkAgentCert(st state.State, now time.Time) checkResult {
	const name = "agent certificate"
	latest, found, err := scanBrokerLogCertExpiry(st.OutLog())
	switch {
	case err != nil:
		return checkResult{name, true, "no broker log to scan", ""}
	case !found:
		return checkResult{name, true, "no expired-leaf rejections in the broker log", ""}
	case now.Sub(latest) <= certRejectWindow:
		// A rejection logged before the current broker started describes the outage
		// the restart remedied.
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

// brokerStartTime is the pid file's mtime (written once, after the listeners
// bind); ok=false without a pid file.
func brokerStartTime(pidPath string) (time.Time, bool) {
	fi, err := os.Stat(pidPath)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// scanBrokerLogCertExpiry returns the time of the latest expired or bad
// certificate handshake error in the broker log's tail. It reads lever's
// daemon log line ("time=<RFC3339, UTC> ...") and the Go-stdlog prefix ("2006/01/02
// 15:04:05 ...", local time) an older log file may hold.
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
		ts, ok := brokerLogLineTime(line)
		if !ok {
			continue
		}
		if !found || ts.After(latest) {
			latest, found = ts, true
		}
	}
	return latest, found, nil
}

func brokerLogLineTime(line string) (time.Time, bool) {
	if rest, ok := strings.CutPrefix(line, "time="); ok {
		if i := strings.IndexByte(rest, ' '); i > 0 {
			rest = rest[:i]
		}
		ts, err := time.Parse(logging.TimeFormat, rest)
		return ts, err == nil
	}
	if len(line) < 19 {
		return time.Time{}, false
	}
	ts, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local)
	return ts, err == nil
}

// Labels image/lever-claude/Dockerfile bakes from build args: the pinned Claude
// Code version and the lever version the in-jail binaries were built from.
const (
	claudeVersionLabel = "claude_code_version"
	leverVersionLabel  = "lever_version"
)

// imageLabelProbe reads one label via `docker image inspect`; "" when absent.
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

// checkClaudeVersion reports the Claude Code version label of the manager
// image, from the image_tar archive or host docker. A missing label is
// informational; an inspect error (image not loaded, tar unreadable or
// mistagged) is a fault.
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

// checkLeverVersion compares the in-jail code baked into the manager image with
// this binary's; the image tag names only the arch.
//
// The lever_jail_abi label (internal/jailabi) decides when present: equal to
// hostABI passes (a release difference is shown, since a release that changed
// no in-jail code needs no rebuild); different fails. Without it the
// lever_version label is compared to host, the release; hostFull is what
// `lever version` prints. Releases are compared, not commits. A missing
// lever_version label or an unreadable image is informational: the
// claude-version row already fails on the latter.
func checkLeverVersion(imageRef, tarPath, host, hostFull string, hostABI int, p doctorProbes) checkResult {
	const name = "agent lever version"
	const rebuildFix = "rebuild the agent image from this lever's source (`make lever-image LEVER_IMAGE_FORCE=1`, then any instance image built FROM it), " +
		"then `lever apply`; a running manager keeps its image until `lever up --fresh --keep-conversation`"
	source := imageRef
	var v, abi string
	var err error
	if tarPath != "" {
		source = imageRef + " (from " + tarPath + ")"
		v, err = p.leverVersionTar(tarPath, imageRef)
		if err == nil && p.jailABITar != nil {
			abi, err = p.jailABITar(tarPath, imageRef)
		}
	} else {
		v, err = p.leverVersion(imageRef)
		if err == nil && p.jailABI != nil {
			abi, err = p.jailABI(imageRef)
		}
	}
	if err != nil {
		return checkResult{name, true, "not checked (could not inspect image " + source + "): " + firstLine(err.Error()), ""}
	}
	v, abi = strings.TrimSpace(v), strings.TrimSpace(abi)
	if abi != "" {
		built := ""
		if v != "" {
			built = ", built from lever " + v
		}
		want := strconv.Itoa(hostABI)
		if abi != want {
			return checkResult{name, false,
				fmt.Sprintf("%s carries jail ABI %s%s but the host lever (%s) has jail ABI %s — its in-jail binaries (lever-agent, lever-manager) do not match this lever", source, abi, built, hostFull, want),
				rebuildFix}
		}
		release := ""
		switch {
		case v == hostFull:
			release = " (the same build as the host)"
		case v != "":
			release = "; host lever is " + hostFull + " (info: the in-jail code is the same, so no rebuild is needed)"
		}
		return checkResult{name, true, fmt.Sprintf("%s carries jail ABI %s%s, the same as the host lever%s", source, abi, built, release), ""}
	}
	if v == "" {
		return checkResult{name, true, "not checked: no " + leverVersionLabel + " label on " + source +
			" — the image predates the label (or was not built by `make lever-image`); rebuild it so this row can compare it with the host lever", ""}
	}
	if release := strings.Fields(v)[0]; release != host {
		return checkResult{name, false,
			fmt.Sprintf("%s carries lever %s but the host lever is %s — its in-jail binaries (lever-agent, lever-manager) were built from another release (the image has no %s label, so the release is compared)", source, v, hostFull, jailabi.Label),
			rebuildFix}
	}
	if v == hostFull {
		return checkResult{name, true, fmt.Sprintf("%s carries lever %s, the same as the host", source, v), ""}
	}
	return checkResult{name, true,
		fmt.Sprintf("%s carries lever %s, host lever is %s — same release %s (the release is compared, not the commit)", source, v, hostFull, host), ""}
}

// checkPATTokens judges the hub tokens on disk by their mint records
// (state.PATRecord): the scope set this lever mints, an expiry outside the
// renew window. A UAT cannot list tokens, so a token without a record is a
// finding (scion's 90-day default, no agent:message). The fix is a re-apply.
func checkPATTokens(ctx context.Context, st state.State, remoteEnabled bool, known scopeKnownFunc, now time.Time) checkResult {
	const name = "hub tokens"
	const fix = "run `lever apply` (it re-mints the token as the controller and revokes the old one, while agents run)"
	type tokenCheck struct {
		label string
		load  func() (string, error)
		rec   func() (state.PATRecord, bool, error)
		want  []string
	}
	crec, _, _ := st.LoadControllerPATRecord()
	checks := []tokenCheck{{"controller", st.LoadControllerPAT, st.LoadControllerPATRecord,
		controllerPATScopes(controllerLifecycle(ctx, known, crec), controllerInbox(ctx, known, crec))}}
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

type roleCeilingReader func(ctx context.Context, project string) (hubapi.RoleCeiling, error)

// checkAgentRoleCeiling reports whether the hub caps agent creates in the
// project at the role lever stamps. An unset ceiling reads as `full`, leaving
// lever's --role stamp the only bound. The setting exists only on a scion with
// roles, so the binary is probed first; an older scion is not a finding. An
// unreachable hub is not a finding; one that answered unusably is.
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

// scionTelemetryEnv is what scion turns settings telemetry.enabled into for
// every agent it starts.
const scionTelemetryEnv = "SCION_TELEMETRY_ENABLED"

// settingsReader returns the jail's ~/.scion/settings.yaml ("" when absent).
type settingsReader func(ctx context.Context) ([]byte, error)

type envReader func(ctx context.Context, ref, key string) (value string, set bool, err error)

// readJailScionSettings reads as the run user: the file the hub and runtime
// broker read.
func readJailScionSettings(jr proc.Runner) settingsReader {
	return func(ctx context.Context) ([]byte, error) {
		res, err := jr.Run(ctx, nil, "sh", "-c", `f="$HOME/`+layout.SettingsRel+`"; if [ -f "$f" ]; then cat "$f"; fi`)
		if err != nil {
			return nil, err
		}
		return []byte(res.Stdout), nil
	}
}

// checkScionTelemetry reports the agents' sciontool telemetry posture: the
// jail settings (config scion.telemetry) and the running manager's env. scion
// enables telemetry by default, and with no destination every Claude hook waits
// out OTLP timeouts, so a trivial turn takes minutes. Settings reach an agent
// only at start, so a manager created before them is what this row names.
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
		// scion-default: the operator's choice, nothing to compare the manager against.
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

// checkHubRuntime reads the container runtime the hub starts agents with. Any
// loopback caller, an agent included, can change it through the system
// runtime route, and every later agent start then fails; each apply sets it
// back.
func checkHubRuntime(ctx context.Context, hc *hubapi.Client) checkResult {
	const name = "hub container runtime"
	rt, err := hc.Runtime(ctx)
	if err != nil {
		if hubAnswered(err) {
			return checkResult{name, false, "could not read it: " + err.Error(),
				"the hub answered, so this is not a down instance — check the controller PAT (`" + stateDirName() + "/`)"}
		}
		return checkResult{name, true, "not checked (hub not reachable): " + err.Error(), ""}
	}
	if rt != apply.AgentRuntime {
		return checkResult{name, false,
			fmt.Sprintf("the hub starts agents with %q, not %s: an agent may have changed it, and agent starts fail until it is set back", rt, apply.AgentRuntime),
			"run `lever apply`: it sets it back as lever-admin"}
	}
	return checkResult{name, true, apply.AgentRuntime, ""}
}

// unaskedExperiments returns the experiments lever pins that rec lists
// neither as off nor as looked for.
func unaskedExperiments(rec state.HubExperimentsRecord) []string {
	var out []string
	for _, n := range pinnedOffNames() {
		if !slices.Contains(rec.Off, n) && !slices.Contains(rec.Asked, n) {
			out = append(out, n)
		}
	}
	return out
}

// checkHubExperiments fails when the hub serves a pinned-off scion experiment
// (pinnedOffExperiments), probed with the controller PAT, and warns when it is
// off only by scion's default because lever never pinned it on this hub. The
// pin is read from hub-experiments.json, so doctor needs no admin call. An
// unreachable hub is not a finding; one that answered unusably is.
func checkHubExperiments(ctx context.Context, st state.State, known scopeKnownFunc, hc *hubapi.Client) checkResult {
	const name = "scion conduit and artifacts"
	rec, found, err := st.LoadHubExperimentsRecord()
	if err != nil {
		return checkResult{name, false, err.Error(), "remove " + stateDirName() + "/hub-experiments.json, then run `lever apply`"}
	}
	on, err := liveExperiments(ctx, hc)
	if err != nil {
		if hubAnswered(err) {
			return checkResult{name, false, "could not probe the hub: " + err.Error(),
				"the hub answered, so this is not a down instance — check the controller PAT (`" + stateDirName() + "/`)"}
		}
		return checkResult{name, true, "not checked (hub not reachable): " + err.Error(), ""}
	}
	if len(on) > 0 {
		return checkResult{name, false,
			strings.Join(on, ", ") + " on at the hub: scion experiments not reviewed for the jail (conduit: an inbound shell and port path into agents; " +
				"artifacts: hub-held publishing with unauthenticated share links; chat thread groups: project-wide chat groups agents can create)",
			"run `lever apply`: it pins it off as lever-admin and restarts the hub"}
	}
	if !hubHasExperiments(ctx, known, rec, found) {
		return checkResult{name, true, "off: the jail's scion has no hub experiments, so none of these features exists", ""}
	}
	if !found || rec.NotApplicable {
		return warnResult(name,
			"off by scion's default only: lever has not pinned them off on this hub, so a changed default or an admin's override would turn them on",
			"run `lever apply`: it pins them off as lever-admin")
	}
	if missing := unaskedExperiments(rec); len(missing) > 0 {
		return warnResult(name,
			"an older lever pinned the experiments on this hub and never looked for "+strings.Join(missing, ", ")+
				", so a changed default or an admin's override would turn it on",
			"run `lever apply`: it pins them off as lever-admin")
	}
	if len(rec.Off) == 0 {
		return checkResult{name, true, "off: this hub registers none of the experiments lever pins", ""}
	}
	var probed, unprobed []string
	for _, e := range pinnedOffExperiments {
		if !slices.Contains(rec.Off, e.name) {
			continue
		}
		if e.probe == "" {
			unprobed = append(unprobed, e.name)
		} else {
			probed = append(probed, e.name)
		}
	}
	detail := fmt.Sprintf("off at the hub; pinned off by lever on %s (%s)", rec.PinnedAt.Format(time.RFC3339), strings.Join(probed, ", "))
	if len(unprobed) > 0 {
		detail += fmt.Sprintf("; %s pinned off on the same apply, which no route can show, so doctor reads only lever's record", strings.Join(unprobed, ", "))
	}
	return checkResult{name, true, detail, ""}
}
