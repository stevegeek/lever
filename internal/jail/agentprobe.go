package jail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// AgentTokenPath is where scion keeps an agent's hub token inside its
// container (scion pkg/sciontool/hub/client.go TokenFilePath).
const AgentTokenPath = "/home/scion/.scion/scion-token"

// agentTokenScript prints the container's clock and only the payload segment
// of the JWT in $1: without the signature the payload is no credential.
// `cut -s` prints nothing for a line without a dot, so a file that is not a
// JWT (or a bare secret) never leaves the container whole. The read is
// bounded.
const agentTokenScript = `date -u +%s && head -c 16384 "$1" | cut -s -d. -f2 | head -c 8192`

// HubTokenTimes is an agent hub token's expiry and the container's clock at
// the read. The container clock is the guest's, which the hub also checks
// expiry against, so the two compare directly even when the guest clock
// drifted from the host's.
type HubTokenTimes struct {
	Expiry time.Time
	Now    time.Time
}

// Expired reports whether the hub refuses the token now. An expired agent
// token cannot refresh itself (sciontool logs AUTH_LOST): every agent-to-hub
// call fails with 401 until a new token is injected.
func (t HubTokenTimes) Expired() bool { return !t.Now.Before(t.Expiry) }

// RefreshOverdue reports a token past its refresh time but not yet expired.
// sciontool's refresh timer counts the guest's monotonic clock, which stands
// still while the host sleeps, so a refresh can be late enough to expire.
func (t HubTokenTimes) RefreshOverdue() bool {
	return !t.Expired() && !t.Now.Before(t.Expiry.Add(-AgentTokenRefreshMargin))
}

// AgentTokenRefreshMargin is how long before expiry sciontool refreshes an
// agent's hub token (scion cmd/sciontool/commands/init.go).
const AgentTokenRefreshMargin = 2 * time.Hour

// AgentUser is the user every probe execs as: scion's own execs run as
// "scion" (scion pkg/runtime/podman.go), the owner of the harness's tmux
// server and the token file, whatever USER the image declares.
const AgentUser = "scion"

// Every exec into an agent container runs a program, or reads a file, the
// agent may have replaced (lever treats the agent as root there; a FIFO makes
// `head` block): each gets its own deadline, so a hostile agent cannot hang
// doctor, apply or the broker, and its output is capped, so it cannot grow
// the host process.
const agentExecOutputLimit = 64 << 10

var agentExecTimeout = 10 * time.Second

// BoundAgentExec returns ctx with the deadline and output cap every exec
// into an agent container runs under. The caller defers cancel.
func BoundAgentExec(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(proc.WithOutputLimit(ctx, agentExecOutputLimit), agentExecTimeout)
}

// AgentProbe reads (and, for ReportSessionRunning, nudges) an agent's
// session from inside its container. Everything it returns is the agent's
// own word: only a time, a boolean or an error, and no error carries text
// from the container.
type AgentProbe struct {
	R proc.Runner
}

// HubToken reads the agent's hub token expiry and the container clock. Only
// the exp claim is decoded, and no error repeats what the container printed:
// the agent can write the file.
func (p AgentProbe) HubToken(ctx context.Context, ref string) (HubTokenTimes, error) {
	if err := checkRef(ref); err != nil {
		return HubTokenTimes{}, fmt.Errorf("reading agent hub token: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := runGuestBounded(ctx, p.R, "podman", "exec", "--user", AgentUser, ref, "sh", "-c", agentTokenScript, "sh", AgentTokenPath)
	if err != nil {
		switch {
		case noSuchContainer(res.Stderr):
			return HubTokenTimes{}, fmt.Errorf("reading agent hub token in %s: %w", ref, ErrNoContainer)
		case errors.Is(err, proc.ErrOutputLimit):
			return HubTokenTimes{}, fmt.Errorf("reading agent hub token in %s: %w", ref, proc.ErrOutputLimit)
		case ctx.Err() != nil, errors.Is(err, ErrProbeTimedOut):
			return HubTokenTimes{}, fmt.Errorf("reading agent hub token in %s: %w", ref, ErrProbeTimedOut)
		}
		return HubTokenTimes{}, fmt.Errorf("reading agent hub token in %s: %w", ref, &ProbeExitError{Code: res.Code})
	}
	return parseHubTokenTimes(res.Stdout)
}

// ErrNoTmuxServer is HarnessAlive's answer when tmux in the container finds
// no server on the socket it tries: scion has not started it yet, it is
// gone, or the exec reached another socket. Not proof either way.
var ErrNoTmuxServer = errors.New("no tmux server answered")

// ErrProbeTimedOut is a probe exec that passed its deadline.
var ErrProbeTimedOut = errors.New("the probe timed out")

// ProbeExitError is a probe exec that exited non-zero. It carries only the
// exit code: the output is the agent's word.
type ProbeExitError struct{ Code int }

func (e *ProbeExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

// ProbeErrorClass names the class of a probe error in fixed words, so an
// operator line repeats nothing the container printed.
func ProbeErrorClass(err error) string {
	var exit *ProbeExitError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoContainer):
		return "no container"
	case errors.Is(err, ErrNoTmuxServer):
		return "no tmux server"
	case errors.Is(err, ErrProbeTimedOut):
		return "timed out"
	case errors.Is(err, proc.ErrOutputLimit):
		return "output over its limit"
	case errors.Is(err, ErrTokenShape), errors.Is(err, ErrClaudeShape), errors.Is(err, ErrSessionsShape), errors.Is(err, ErrReadyShape):
		return "unexpected output"
	case errors.As(err, &exit):
		return exit.Error()
	}
	return "error"
}

// ErrTokenShape is every way the probe's output can fail to parse. It names
// no part of the output.
var ErrTokenShape = errors.New("reading agent hub token: unexpected output (no clock line, or a token without a readable exp claim)")

func parseHubTokenTimes(out string) (HubTokenTimes, error) {
	clock, payload, ok := strings.Cut(strings.TrimSpace(out), "\n")
	if !ok {
		return HubTokenTimes{}, ErrTokenShape
	}
	now, err := strconv.ParseInt(strings.TrimSpace(clock), 10, 64)
	if err != nil {
		return HubTokenTimes{}, ErrTokenShape
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(payload), "="))
	if err != nil {
		return HubTokenTimes{}, ErrTokenShape
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return HubTokenTimes{}, ErrTokenShape
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return HubTokenTimes{}, ErrTokenShape
	}
	return HubTokenTimes{Expiry: time.Unix(exp, 0).UTC(), Now: time.Unix(now, 0).UTC()}, nil
}

// HarnessAlive reports whether the pane of scion's `agent` tmux window is
// alive: scion runs claude in that window (the `shell` window keeps the
// session alive), so the window goes away when claude exits. Only tmux
// saying it cannot find the window or session is a false; any other failure
// is an error.
func (p AgentProbe) HarnessAlive(ctx context.Context, ref string) (bool, error) {
	if err := checkRef(ref); err != nil {
		return false, fmt.Errorf("probing agent harness: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := runGuestBounded(ctx, p.R, "podman", "exec", "--user", AgentUser, ref, "tmux", "list-panes", "-t", "scion:agent", "-F", "#{pane_dead}")
	if err == nil {
		for _, ln := range strings.Fields(res.Stdout) {
			if ln == "0" {
				return true, nil
			}
		}
		return false, nil
	}
	s := strings.ToLower(res.Stderr)
	switch {
	case errors.Is(err, ErrProbeTimedOut):
		return false, fmt.Errorf("probing agent harness in %s: %w", ref, ErrProbeTimedOut)
	case noSuchContainer(res.Stderr):
		return false, fmt.Errorf("probing agent harness in %s: %w", ref, ErrNoContainer)
	case strings.Contains(s, "can't find window"), strings.Contains(s, "can't find session"):
		return false, nil
	}
	// "no server running" and "error connecting to" also mean this exec
	// reached another socket than scion's (another uid or TMUX_TMPDIR), so
	// they are not proof the harness exited. A distinct error lets the
	// manager gate tell "not started yet" from a probe that could not run.
	if strings.Contains(s, "no server running") || strings.Contains(s, "error connecting to") {
		return false, fmt.Errorf("probing agent harness in %s: %w", ref, ErrNoTmuxServer)
	}
	return false, fmt.Errorf("probing agent harness in %s: exit status %d", ref, res.Code)
}

// ReportSessionRunning makes the agent report its own session running to the
// hub, as Claude Code's SessionStart hook does, with the agent's own hub
// token. Nothing new enters the container. sciontool exits 0 even when the
// hub call fails, so the caller re-reads the phase to learn whether it
// worked.
func (p AgentProbe) ReportSessionRunning(ctx context.Context, ref string) error {
	if err := checkRef(ref); err != nil {
		return fmt.Errorf("reporting agent session: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := runGuestBounded(ctx, p.R, "podman", "exec", "--user", AgentUser, ref, "sciontool", "hook", "--dialect=claude", "SessionStart")
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return fmt.Errorf("reporting agent session in %s: %w", ref, ErrNoContainer)
		}
		return fmt.Errorf("reporting agent session in %s: exit status %d", ref, res.Code)
	}
	return nil
}

// checkRef refuses a container reference podman would read as a flag.
func checkRef(ref string) error {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid container reference %q", ref)
	}
	return nil
}

func noSuchContainer(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object")
}

// AttachSession is scion's tmux session in an agent container, the one
// `scion attach` attaches to (scion pkg/runtime/podman.go).
const AttachSession = "scion"

// detachClientsScript detaches the tmux clients of the scion session that
// a `podman exec` started, and prints one "d" line for each. $1 is the /proc
// root (a test seam).
//
// It must spare the container's own client: under podman scion's start
// command ends in a foreground `attach-session -t scion`, the container's
// main process, and detaching it would stop the agent. An exec'd process has
// no parent inside the container's pid namespace, so its status reads
// "PPid: 0"; only those are detached. PID 1 also reads PPid 0 and is never
// detached. Anything unreadable is left attached.
const detachClientsScript = `clients=$(tmux list-clients -t ` + AttachSession + ` -F '#{client_pid} #{client_tty}' 2>/dev/null) || exit 0
printf '%s\n' "$clients" | while read -r pid tty; do
  case "$pid" in ''|*[!0-9]*|1) continue ;; esac
  [ -n "$tty" ] || continue
  ppid=
  while read -r k v; do
    if [ "$k" = PPid: ]; then ppid=$v; break; fi
  done 2>/dev/null < "$1/$pid/status" || continue
  [ "$ppid" = 0 ] || continue
  tmux detach-client -t "$tty" </dev/null 2>/dev/null && echo d
done
exit 0`

// DetachClients detaches the tmux clients that earlier attaches left on the
// agent's scion session and returns how many. A stale client whose terminal
// went away can freeze or misdraw the next attach. Only a count leaves the
// container.
func (p AgentProbe) DetachClients(ctx context.Context, ref string) (int, error) {
	if err := checkRef(ref); err != nil {
		return 0, fmt.Errorf("detaching tmux clients: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := p.R.Run(ctx, nil, "podman", "exec", "--user", AgentUser, ref, "sh", "-c", detachClientsScript, "sh", "/proc")
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return 0, fmt.Errorf("detaching tmux clients in %s: %w", ref, ErrNoContainer)
		}
		return 0, fmt.Errorf("detaching tmux clients in %s: exit status %d", ref, res.Code)
	}
	n := 0
	for _, f := range strings.Fields(res.Stdout) {
		if f == "d" {
			n++
		}
	}
	return n, nil
}
