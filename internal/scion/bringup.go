package scion

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// AlreadyRunning reports whether err is a scion "already running" error, so
// bring-up steps stay idempotent on re-apply.
func AlreadyRunning(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "already running") || strings.Contains(s, "already exists")
}

// notRunning reports whether err is scion server stop finding no server
// daemon (cmd/server_daemon.go). It is separate from AlreadyRunning, which
// also guards ServerStart, where "not running" must stay a failure.
//
// It matches the full "server daemon is not running": looser forms would also
// swallow scion's agent-level and runtime-broker "not running" messages.
func notRunning(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "server daemon is not running")
}

// startGaveUpWaiting reports whether err is scion server start giving up its
// own /healthz wait (about 30 s, cmd/server_start_ready.go) while the daemon
// it launched is still running. That daemon owns the pid file and may only be
// slow, so the start counts as done and lever's longer waitHubReady decides.
// A daemon that exited during the wait is worded differently and stays a
// failure.
func startGaveUpWaiting(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "server did not become ready within") &&
		strings.Contains(s, "The server process is still running")
}

// IsBrokerUnavailable reports whether err is the bring-up race where the hub
// serves before its runtime broker has registered. scion words it three ways:
//   - scion start: "No runtime brokers available".
//   - scion resume: "no runtime broker available".
//   - either, on a cold VM: "deadline exceeded", when scion's own wait for
//     the broker times out.
//
// A caller that retries on it must check its own ctx between attempts, since
// a deadline from that ctx matches too.
func IsBrokerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no_runtime_broker") ||
		strings.Contains(s, "No runtime brokers available") ||
		strings.Contains(s, "no runtime broker available") ||
		strings.Contains(s, "deadline exceeded")
}

// IsRefusedByHub reports whether err from a scion agent verb is the hub
// REFUSING the caller, as opposed to the agent being broken. A resume that
// fails this way says nothing about the agent's state, so it must never be
// "repaired" by deleting the agent. It matches, case-insensitively:
//   - "already exists in this project": since scion f7155ecb a user token
//     without agent:lifecycle that resumes an existing agent gets this 409;
//   - "(status: 403)", "missing required scope", "token does not have
//     scope": a plain authorization denial.
func IsRefusedByHub(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists in this project") ||
		strings.Contains(msg, "(status: 403)") ||
		strings.Contains(msg, "missing required scope") ||
		strings.Contains(msg, "token does not have scope")
}

// RefusalHint is what to do after IsRefusedByHub. It names both causes: the
// same 409 also answers a resume of a record in the wrong phase (running,
// starting, stopping, error), so the missing scope is not certain.
const RefusalHint = "Two causes are possible. (1) On scion f7155ecb or later, a controller token without " +
	"agent:lifecycle: run `lever stop`, then `lever up` (lever re-mints the token). (2) The manager is in a phase " +
	"that cannot be resumed now (running, starting, stopping or error): wait, check `lever doctor`, then run `lever up` again. " +
	"To discard the session instead, run `lever up --fresh`"

// ErrSummary reduces a scion CLI error to the lines that say what went wrong,
// dropping settings warnings and cobra's usage dump, on one bounded line. It
// returns plain text; the caller sanitizes it for the terminal.
func ErrSummary(err error) string {
	if err == nil {
		return ""
	}
	const maxSummary = 600
	var kept []string
	for _, line := range strings.FieldsFunc(err.Error(), func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "Usage:"):
			return boundSummary(strings.Join(kept, " "), maxSummary)
		case strings.Contains(line, "settings file contains unrecognized keys"):
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return "scion gave no cause"
	}
	return boundSummary(strings.Join(kept, " "), maxSummary)
}

func boundSummary(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…"
}

// IsAgentAbsent reports whether err from a scion agent verb definitively means
// the named agent cannot be running, as opposed to an unknown failure a caller
// must not paper over. It matches, case-insensitively:
//   - "is not responding" / "connection refused": no hub is reachable, and
//     only apply starts one;
//   - "project not found": the hub never registered the project;
//   - "no git origin remote found": scion's fallback when the path is not a
//     locally registered project; lever projects are never git-resolved, so
//     this means not registered.
func IsAgentAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not responding") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "project not found") ||
		strings.Contains(msg, "no git origin remote found")
}

// The default waitHubReady budget: probe every hubReadyInterval for up to
// hubReadyTimeout; say so after hubReadyNotice and then every hubReadyEvery;
// check every hubAliveEvery that a scion server is still alive to wait on.
//
// The timeout is a time, not an attempt count, and long: a cold hub start
// after a reboot (daemon re-exec, cold page cache, a large hub.db) can take
// many seconds before it serves at all.
const (
	hubReadyTimeout  = 2 * time.Minute
	hubReadyInterval = 1 * time.Second
	hubReadyNotice   = 15 * time.Second
	hubReadyEvery    = 30 * time.Second
	hubAliveEvery    = 5 * time.Second
)

// waitHubReady polls a project-independent hub call until it succeeds or the
// budget runs out. list --all does not resolve a project, unlike list
// --global, which fails with "no git origin remote found" before any project
// is registered.
//
// An authentication failure also counts as ready: the hub answered, it only
// refused the credential, which happens whenever lever holds no working token
// yet. Whoever needs the credential fails on it next, with the real error.
//
// Every hubAliveEvery it runs serverPIDProbe, and stops with ErrHubNotReady
// once no scion server is alive, so the long budget costs nothing when the
// start failed. A probe that cannot run says nothing. notify, when non-nil,
// gets progress lines so a slow start does not read as a hang.
func (c *Client) waitHubReady(ctx context.Context, notify func(format string, args ...any)) error {
	// A progress list shows the wait on its live line instead.
	rep := progress.From(ctx)
	if rep.Active() {
		notify = nil
	}
	start := time.Now()
	deadline := start.Add(c.hubReadyTimeout)
	nextNotice := start.Add(c.hubReadyNotice)
	lastAlive := start
	attempts := 0
	var lastErr error
	errGone := errors.New("server gone")
	// One jail call polls /healthz for up to hubAliveEvery; the scion CLI
	// probe after each in-guest wait still decides readiness. Without an
	// endpoint, or when the in-guest wait cannot run, it polls with the CLI
	// every hubReadyInterval.
	inGuest := c.hubEndpoint != ""
	pace := false
	err := retry.Until(ctx, 0, 0, func() (bool, error) {
		attempts++
		if pace {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(c.hubReadyInterval):
			}
		}
		pace = true
		if inGuest {
			switch c.awaitHealthz(ctx, min(c.hubAliveEvery, time.Until(deadline))) {
			case healthzDown:
				pace = false // the guest loop has already waited
			case healthzUnavailable:
				inGuest = false
			}
		}
		_, lastErr = c.run(ctx, "", "list", "--all", "--format", "json", nonInteractive)
		if lastErr == nil || isHubAuthRefusal(lastErr) {
			return true, nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			return false, retry.ErrExhausted
		}
		if now.Sub(lastAlive) >= c.hubAliveEvery {
			lastAlive = now
			if pid, perr := c.serverPIDProbe(ctx); perr == nil && pid == 0 {
				return false, errGone
			}
		}
		rep.Detail(fmt.Sprintf("hub not answering yet; a cold start can take a minute (waiting up to %s)", c.hubReadyTimeout))
		if notify != nil && !now.Before(nextNotice) {
			notify("scion-server: the hub is not answering yet after %s; a cold start (the first after a reboot or a scion pin change) can take a minute — waiting up to %s",
				now.Sub(start).Round(time.Second), c.hubReadyTimeout)
			nextNotice = now.Add(hubReadyEvery)
		}
		return false, nil
	})
	switch {
	case errors.Is(err, retry.ErrExhausted):
		return fmt.Errorf("%w after %s (%d attempts): %w", ErrHubNotReady, time.Since(start).Round(time.Second), attempts, lastErr)
	case errors.Is(err, errGone):
		return fmt.Errorf("%w: no scion server is running in the jail any more (it exited during start-up); see ~/%s in the jail: %w",
			ErrHubNotReady, layout.ServerLogRel, lastErr)
	}
	return err
}

// healthzScript exits 0 once /healthz answers 200, waitedExit when it never
// did, 127 without curl, and 4 at once when something answers but not with a
// 200 or not as plain HTTP, so a hub without /healthz does not cost every
// attempt the whole window. Each request is bounded, so a hub that accepts
// and hangs cannot stretch the loop.
const healthzScript = `command -v curl >/dev/null 2>&1 || exit 127
i=0
while :; do
  curl -fs -o /dev/null --connect-timeout 1 --max-time 2 "$1/healthz"
  case $? in 0) exit 0 ;; 22|35|60) exit 4 ;; esac
  [ "$i" -ge "$2" ] && exit 3
  sleep 0.15
  i=$((i+1))
done`

// waitedExit is how both in-guest waits say they polled for their whole
// window without success. A code of its own, so a jail transport failure or a
// shell that could not start is never read as "not ready yet".
const waitedExit = 3

type healthzResult int

const (
	healthzUp          healthzResult = iota
	healthzDown                      // polled for the whole window, no answer
	healthzUnavailable               // the in-guest poll could not run
)

// awaitHealthz: only waitedExit means "not up yet"; any other failure means
// the in-guest path is unusable here.
func (c *Client) awaitHealthz(ctx context.Context, d time.Duration) healthzResult {
	n := int(d / (150 * time.Millisecond))
	res, err := c.r.Run(ctx, nil, "sh", "-c", healthzScript, "_", strings.TrimRight(c.hubEndpoint, "/"), strconv.Itoa(n))
	switch {
	case err == nil:
		return healthzUp
	case res.Code == waitedExit:
		return healthzDown
	}
	return healthzUnavailable
}

// isHubAuthRefusal reports whether a scion CLI error is the hub refusing the
// credential (HTTP 401, worded by scion cmd/common.go), which proves the hub
// is up.
func isHubAuthRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "authentication failed, login to hub")
}

// ErrHubNotReady is wrapped by waitHubReady when the hub never answers within
// its budget, or its server is gone; the last probe error is wrapped
// alongside it.
var ErrHubNotReady = errors.New("hub not ready")

// brokerReadyAttempts/brokerReadyInterval are the default WaitRuntimeBrokerReady
// budget: their product is how long it waits.
const (
	brokerReadyAttempts = 30
	brokerReadyInterval = 1 * time.Second
)

// brokerPollEvery is how often the in-guest broker wait asks the hub.
const brokerPollEvery = 200 * time.Millisecond

// runtimeBroker: a broker registers with the hub before it finishes
// connecting, so a row existing is not enough; readiness needs one that is
// online or connected.
type runtimeBroker struct {
	Status          string `json:"status"`
	ConnectionState string `json:"connectionState"`
}

func (b runtimeBroker) ready() bool {
	return b.Status == "online" || b.ConnectionState == "connected"
}

// brokersOnline reports whether hub brokers output lists a ready broker. A
// parse miss is "not ready".
func brokersOnline(out string) bool {
	var brokers []runtimeBroker
	if parseJSON(out, &brokers) != nil {
		return false
	}
	return slices.ContainsFunc(brokers, runtimeBroker.ready)
}

// brokerWaitScript polls scion hub brokers until its output names an online
// or connected broker, and prints that output; waitedExit when none did. The
// text match only decides when to stop: lever parses the output itself.
const brokerWaitScript = `n=$1; shift
i=0
while :; do
  if out=$("$@" 2>/dev/null); then
    case $out in *'"online"'*|*'"connected"'*) printf '%s' "$out"; exit 0 ;; esac
  fi
  [ "$i" -ge "$n" ] && exit 3
  sleep 0.2
  i=$((i+1))
done`

// WaitRuntimeBrokerReady blocks until the hub reports at least one ONLINE
// runtime broker, or the budget is exhausted; see RuntimeBrokerReady, whose
// answer it drops.
func (c *Client) WaitRuntimeBrokerReady(ctx context.Context, project string) error {
	_, err := c.RuntimeBrokerReady(ctx, project)
	return err
}

// RuntimeBrokerReady blocks until the hub reports at least one online runtime
// broker, or the budget is exhausted, and reports whether it saw one. The
// runtime broker registers and connects asynchronously after the Hub API
// serves, and scion start and resume need it.
//
// The wait runs in the guest; when that cannot run, it polls from the host.
//
// Fail-soft: on budget exhaustion it returns (false, nil) and the caller
// starts anyway, with the start path's IsBrokerUnavailable retry as the
// backstop. Only ctx cancellation returns an error. project is passed only to
// avoid scion's "no project" resolution failure; hub brokers is hub-wide.
func (c *Client) RuntimeBrokerReady(ctx context.Context, project string) (bool, error) {
	args := append([]string{"hub", "brokers", "--format", "json", nonInteractive}, projectFlag(project)...)
	budget := time.Duration(c.brokerReadyAttempts) * c.brokerReadyInterval
	deadline := time.Now().Add(budget)
	for {
		n := int(time.Until(deadline) / brokerPollEvery)
		argv := append([]string{"-c", brokerWaitScript, "_", strconv.Itoa(max(n, 0)), c.bin}, args...)
		res, err := c.r.RunIn(ctx, "", c.env(), "sh", argv...)
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
		if err == nil && brokersOnline(res.Stdout) {
			return true, nil
		}
		if err != nil && res.Code != waitedExit {
			break // the in-guest wait cannot run here: poll from the host
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		// The guest matched text that lever's parse did not accept: keep
		// waiting, without spinning.
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(brokerPollEvery):
		}
	}
	attempts := c.brokerReadyAttempts
	if c.brokerReadyInterval > 0 {
		attempts = max(1, int(time.Until(deadline)/c.brokerReadyInterval))
	}
	online := false
	err := retry.Until(ctx, attempts, c.brokerReadyInterval, func() (bool, error) {
		out, err := c.runValue(ctx, "", args...)
		if err != nil {
			return false, nil
		}
		online = brokersOnline(out)
		return online, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return false, nil
	}
	return online, err
}

// InitMachine seeds the machine-level scion dir and default harness configs.
// Required before --harness claude resolves.
func (c *Client) InitMachine(ctx context.Context) error {
	_, err := c.run(ctx, "", "init", "--machine", "--non-interactive")
	return err
}

// ConfigSetGlobal sets a global scion config key (e.g. image_registry=scionlocal).
func (c *Client) ConfigSetGlobal(ctx context.Context, key, value string) error {
	_, err := c.run(ctx, "", "config", "set", "--global", nonInteractive, key, value)
	return err
}

// ConfigGetProject reads an effective scion config key as resolved FOR a
// project — the merge of embedded defaults, the machine settings, and the
// project's own, in that precedence.
func (c *Client) ConfigGetProject(ctx context.Context, projectDir, key string) (string, error) {
	// runValue: the result is compared against an expected value.
	return c.runValue(ctx, projectDir, "config", "get", nonInteractive, key)
}

// ConfigSetProject sets a scion config key at PROJECT scope, resolved from
// projectDir as the working directory.
//
// Project scope is the one that wins: scion merges embedded defaults, then
// machine, then project settings, and writes default_template: default into
// the project's own settings at registration.
func (c *Client) ConfigSetProject(ctx context.Context, projectDir, key, value string) error {
	_, err := c.run(ctx, projectDir, "config", "set", nonInteractive, key, value)
	return err
}

// ServerOpts configures `scion server start`.
type ServerOpts struct {
	// WebPort, when > 0, sets the port the Hub API is reachable on
	// (--web-port). In workstation mode with the web frontend on, the Hub API
	// is mounted on the web port and --port is ignored. Zero means scion's
	// default.
	WebPort int
	// DevAuth is always emitted explicitly (--dev-auth=true|false) so the real
	// hub is never left on the (dev-auth-on) default by omission.
	DevAuth bool
	// EnableWeb passes --enable-web, for remote access only. A hub started
	// without it still has the web frontend: scion's workstation defaults
	// enable it unless --enable-web=false is explicit. The live hub must
	// never pass that: with web disabled the Hub API moves off --web-port,
	// where the broker, the agents, doctor and the remote proxy all dial it.
	// So in practice this field decides only WebAssetsDir.
	//
	// There is deliberately no --base-url. scion also makes it the hub
	// endpoint injected into every agent container, and a non-loopback
	// endpoint is unreachable from the jail and skips scion's
	// container-bridge rewrite, breaking the agents' hub callbacks. The SPA
	// builds every URL relative and does not need it.
	EnableWeb bool
	// WebAssetsDir, when non-empty, serves the SPA from this guest directory
	// (--web-assets-dir). A binary built from a scion.version pin embeds no
	// SPA, so lever stages the assets itself.
	//
	// Set it only with EnableWeb and only when lever staged the assets: scion
	// serves a non-empty value whether or not it holds anything, so a wrong
	// path replaces working embedded assets with 404s.
	WebAssetsDir string
	// SessionSecret, when non-empty, is the hub's session-cookie signing key
	// (--session-secret); without it every hub restart signs every browser
	// out. The flag, not env, because scion's daemon persists and replays its
	// argv on restart. Empty omits the flag.
	SessionSecret string
	// Env is set on the start command only, and the daemonised hub inherits it.
	// It pins the hub's auth configuration: scion lets SCION_SERVER_* override
	// settings.yaml, which agents can rewrite through the workstation routes.
	// The values are configuration, never credentials: the jail runner puts env
	// on the host argv.
	Env map[string]string
	// Progress, when non-nil, gets a line while the start waits on a slow
	// hub (see waitHubReady). Not part of the argv.
	Progress func(format string, args ...any)
}

// ServerStart starts the workstation daemon (Hub API + broker); it daemonises
// and returns once waitHubReady sees the hub answer.
//
// The options describe a start, not a desired state: a running daemon keeps
// its argv, and this call tolerates scion's "already running" refusal. A
// caller that needs a changed option to take effect must ServerStop first.
//
// scion's own readiness give-up (startGaveUpWaiting) is not a failure: the
// daemon keeps starting, and waitHubReady decides.
func (c *Client) ServerStart(ctx context.Context, o ServerOpts) error {
	args := []string{"server", "start"}
	if o.WebPort > 0 {
		args = append(args, "--web-port", strconv.Itoa(o.WebPort))
	}
	args = append(args, fmt.Sprintf("--dev-auth=%t", o.DevAuth))
	// Equals form, so the argv matches scion's own daemon re-exec form.
	if o.SessionSecret != "" {
		args = append(args, "--session-secret="+o.SessionSecret)
	}
	if o.EnableWeb {
		args = append(args, "--enable-web")
		// Equals form, as scion's own daemon re-exec writes it.
		if o.WebAssetsDir != "" {
			args = append(args, "--web-assets-dir="+o.WebAssetsDir)
		}
	}
	// A pid file naming a live non-scion process (a reboot reused the pid)
	// makes scion answer "already running" without starting anything. The
	// probe removes such a file first; best-effort.
	_, _ = c.serverPIDProbe(ctx)
	// runSecret: the argv carries the session secret, which redactArgs does
	// not know. The scrub is textual, so AlreadyRunning still matches.
	if _, err := c.runSecretEnv(ctx, o.Env, o.SessionSecret, args...); err != nil && !startGaveUpWaiting(err) && !AlreadyRunning(err) {
		return err
	}
	return c.waitHubReady(ctx, o.Progress)
}

// ServerStop stops the workstation daemon, tolerating one that is not running
// (notRunning, not AlreadyRunning) so callers can call it unconditionally.
//
// Two guards wrap the stop:
//
//   - Before it, serverPIDProbe: scion signals whatever pid the pid file
//     names after only a signal-0 check, so after a reboot a stale file can
//     name an unrelated process of the jail user. Such a file is removed
//     instead of being signalled.
//   - After it, waitPIDGone: scion's stop returns after a fixed 500 ms,
//     possibly before the process has released its ports.
//
// The probe is best-effort: if the jail shell fails, the stop runs without
// the wait.
func (c *Client) ServerStop(ctx context.Context) error {
	pid, _ := c.serverPIDProbe(ctx)
	if _, err := c.run(ctx, "", "server", "stop"); err != nil && !AlreadyRunning(err) && !notRunning(err) {
		return err
	}
	if pid > 0 {
		return c.waitPIDGone(ctx, pid)
	}
	return nil
}

// serverPIDProbeScript prints the pid the server pid file names when it is a
// live scion server, nothing when there is no file or the pid is dead, and
// removes the file when the pid is alive but not a scion server. The cmdline
// test matches scion's own daemon argv (cmd/server_daemon.go).
const serverPIDProbeScript = `f="$HOME/$1"
[ -f "$f" ] || exit 0
pid=$(tr -cd '0-9' < "$f")
[ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ] || exit 0
case "$(tr '\000' ' ' < "/proc/$pid/cmdline")" in
*scion*" server "*) printf '%s' "$pid" ;;
*) rm -f -- "$f"; printf 'stale:%s' "$pid" ;;
esac`

// serverPIDProbe returns the live scion server's pid, or 0 when there is none.
func (c *Client) serverPIDProbe(ctx context.Context) (int, error) {
	res, err := c.r.Run(ctx, nil, "sh", "-c", serverPIDProbeScript, "_", layout.ServerPIDRel)
	if err != nil {
		return 0, err
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" || strings.HasPrefix(out, "stale:") {
		return 0, nil
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("server pid probe printed %q", out)
	}
	return pid, nil
}

// RunningHub is what the live hub daemon was started with: its argv and the
// part of its environment lever pins (HubEnvPrefixes). Running is false when no
// scion server runs.
type RunningHub struct {
	Running bool
	PID     int
	Argv    []string
	Env     map[string]string
}

// HubEnvPrefixes are the environment keys RunningHub reports. Only these
// leave the jail: the daemon's environment can hold the session secret and a
// hub token.
var HubEnvPrefixes = []string{"SCION_SERVER_AUTH_", "SCION_SERVER_HUB_ADMINEMAILS", "LEVER_HUB_"}

// runningHubScript prints the server's pid, then its NUL-separated argv, a
// separator line, then the allowed env entries. The env filter runs in the
// guest, so nothing else of the daemon's environment is ever read out.
var runningHubScript = `f="$HOME/$1"
[ -f "$f" ] || exit 0
pid=$(tr -cd '0-9' < "$f")
[ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ] || exit 0
case "$(tr '\000' ' ' < "/proc/$pid/cmdline")" in
*scion*" server "*) ;;
*) exit 0 ;;
esac
printf '%s\n' "$pid"
tr '\000' '\n' < "/proc/$pid/cmdline"
printf '%s\n' '--lever-env--'
tr '\000' '\n' < "/proc/$pid/environ" | grep -E '^(` + strings.Join(hubEnvPatterns(), "|") + `)' | grep -v '^SCION_SERVER_SESSION' || true
`

func hubEnvPatterns() []string {
	out := make([]string, len(HubEnvPrefixes))
	for i, p := range HubEnvPrefixes {
		out[i] = regexp.QuoteMeta(p)
	}
	return out
}

// RunningHub reads the live hub's argv and pinned env from /proc in the jail.
func (c *Client) RunningHub(ctx context.Context) (RunningHub, error) {
	res, err := c.r.Run(ctx, nil, "sh", "-c", runningHubScript, "_", layout.ServerPIDRel)
	if err != nil {
		return RunningHub{}, fmt.Errorf("reading the running hub: %w", err)
	}
	return parseRunningHub(res.Stdout)
}

func parseRunningHub(out string) (RunningHub, error) {
	first, rest, _ := strings.Cut(out, "\n")
	first = strings.TrimSpace(first)
	if first == "" || strings.HasPrefix(first, "stale:") {
		return RunningHub{}, nil
	}
	pid, err := strconv.Atoi(first)
	if err != nil {
		return RunningHub{}, fmt.Errorf("running hub probe printed %q", first)
	}
	h := RunningHub{Running: true, PID: pid, Env: map[string]string{}}
	argv, env, ok := strings.Cut(rest, "--lever-env--\n")
	if !ok {
		return RunningHub{}, fmt.Errorf("running hub probe: no environment section")
	}
	for _, a := range strings.Split(strings.TrimSuffix(argv, "\n"), "\n") {
		if a != "" {
			h.Argv = append(h.Argv, a)
		}
	}
	for _, kv := range strings.Split(env, "\n") {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			h.Env[k] = v
		}
	}
	return h, nil
}

const serverExitWait = 15 * time.Second

const waitPIDGoneScript = `i=0
while [ -e "/proc/$1" ]; do
  [ "$i" -ge "$2" ] && exit 1
  sleep 0.1
  i=$((i+1))
done`

func (c *Client) waitPIDGone(ctx context.Context, pid int) error {
	tenths := int(serverExitWait / (100 * time.Millisecond))
	if _, err := c.r.Run(ctx, nil, "sh", "-c", waitPIDGoneScript, "_", strconv.Itoa(pid), strconv.Itoa(tenths)); err != nil {
		return fmt.Errorf("scion server stop: the hub (pid %d) was still running %s after the stop: %w", pid, serverExitWait, err)
	}
	return nil
}

// SecretSet stores a Hub secret that is always projected into the agent
// container. It uses hub env set --secret, not hub secret set: both write the
// same row, but only the env form can set the injection mode, and scion
// filters an as_needed value out of the projected container env.
//
// The value goes over in plaintext: since scion ce96122c the CLI stamps
// encoding=raw on every non-@file value. Encoding it here would hand the
// agent a corrupt credential. An older pin fails loudly (errBase64Pin).
func (c *Client) SecretSet(ctx context.Context, key, value string) error {
	_, err := c.runSecret(ctx, value, "hub", "env", "set", "--secret", "--always", nonInteractive, key, value)
	return secretSetErr(key, err)
}

// secretSetErr reports the cause rather than the symptom: the hub rejects a
// plaintext value only on a pin that predates the CLI's encoding=raw flip.
func secretSetErr(key string, err error) error {
	if err == nil {
		return nil
	}
	if isBase64Rejection(err) {
		return fmt.Errorf("%w (setting %s): %v", errBase64Pin, key, err)
	}
	return err
}

// isBase64Rejection reports whether err is the hub refusing a plaintext secret
// value (wording from scion's hub env-set handler).
func isBase64Rejection(err error) bool {
	return err != nil && strings.Contains(err.Error(), "value must be base64-encoded")
}

var errBase64Pin = errors.New(
	"scion rejected a plaintext secret value: this scion pin predates ce96122c " +
		"(#1111, 2026-08-10) — raise scion.source/binary to that commit or later, " +
		"or scion.version to dbf52f22 or later (commits between 4c045fc8 and " +
		"dbf52f22 cannot be fetched through the Go module proxy at all)")

// EnvSet sets a non-secret Hub env var scoped to one agent's project (bare
// --project infers it from projectDir as cwd), so it does not leak to other
// agents. projectDir must be a registered project's dir.
//
// --always is needed as in SecretSet: an as_needed variable no harness
// declares is never delivered.
func (c *Client) EnvSet(ctx context.Context, projectDir, key, value string) error {
	_, err := c.run(ctx, projectDir, "hub", "env", "set", "--project", "--always", nonInteractive, key+"="+value)
	return err
}
