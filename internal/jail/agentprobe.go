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
// container: the scion user's home, .scion/scion-token (scion
// pkg/sciontool/hub/client.go TokenFilePath). sciontool init reads it at
// boot, its refresh loop rewrites it, and the broker's reset-auth handler
// replaces it.
const AgentTokenPath = "/home/scion/.scion/scion-token"

// agentTokenScript prints the container's clock (Unix seconds) and then the
// payload segment of the agent's hub token — the middle of the three JWT
// segments, and nothing else. The header and the signature stay in the
// container: without the signature the payload is no credential, only the
// claims (agent id, project, scopes, expiry). The read is bounded.
const agentTokenScript = `date -u +%s && head -c 16384 ` + AgentTokenPath + ` | cut -d. -f2 | head -c 8192`

// HubTokenTimes is what lever reads of an agent's hub token: its expiry and
// the container's clock at the moment of the read. The container clock is
// the guest kernel's, which is also the hub's (the hub runs in the guest and
// checks expiry against its own clock), so the two compare directly even
// when the guest clock drifted from the host's.
type HubTokenTimes struct {
	Expiry time.Time
	Now    time.Time
}

// Expired reports whether the hub refuses the token now. An expired agent
// token cannot refresh itself (sciontool logs AUTH_LOST), so this is the
// fault, not a transient: every agent-to-hub call fails with 401 until
// something injects a new token.
func (t HubTokenTimes) Expired() bool { return !t.Now.Before(t.Expiry) }

// RefreshOverdue reports a token past its refresh time but not yet expired.
// sciontool refreshes 2 h before expiry with a Go timer, and a Go timer counts
// the guest's monotonic clock, which stands still while the host sleeps; a
// refresh that is late by more than the remaining margin becomes an expiry.
func (t HubTokenTimes) RefreshOverdue() bool {
	return !t.Expired() && !t.Now.Before(t.Expiry.Add(-AgentTokenRefreshMargin))
}

// AgentTokenRefreshMargin is how long before expiry sciontool refreshes an
// agent's hub token (scion cmd/sciontool/commands/init.go).
const AgentTokenRefreshMargin = 2 * time.Hour

// AgentProbe reads (and, for ReportSessionRunning, nudges) an agent's
// session from inside its container, through the jail. Every call is a
// `podman exec` into a container the agent controls, so everything it
// returns is the agent's own word: it yields only a time, a boolean or an
// error, and no error carries text from the container.
type AgentProbe struct {
	R proc.Runner
}

// HubToken reads the agent's hub token expiry and the container clock. Only
// the exp claim is decoded, and no error repeats what the container printed
// (the payload is the agent's claims, and the agent can write the file).
// ErrNoContainer when podman knows no such container.
func (p AgentProbe) HubToken(ctx context.Context, ref string) (HubTokenTimes, error) {
	if err := checkRef(ref); err != nil {
		return HubTokenTimes{}, fmt.Errorf("reading agent hub token: %w", err)
	}
	res, err := p.R.Run(ctx, nil, "podman", "exec", ref, "sh", "-c", agentTokenScript)
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return HubTokenTimes{}, fmt.Errorf("reading agent hub token in %s: %w", ref, ErrNoContainer)
		}
		return HubTokenTimes{}, fmt.Errorf("reading agent hub token in %s: exit status %d", ref, res.Code)
	}
	return parseHubTokenTimes(res.Stdout)
}

// errTokenShape is every way the probe's output can fail to parse. It names
// no part of the output.
var errTokenShape = errors.New("reading agent hub token: unexpected output (no clock line, or a token without a readable exp claim)")

// parseHubTokenTimes decodes agentTokenScript's two lines.
func parseHubTokenTimes(out string) (HubTokenTimes, error) {
	clock, payload, ok := strings.Cut(strings.TrimSpace(out), "\n")
	if !ok {
		return HubTokenTimes{}, errTokenShape
	}
	now, err := strconv.ParseInt(strings.TrimSpace(clock), 10, 64)
	if err != nil {
		return HubTokenTimes{}, errTokenShape
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(payload), "="))
	if err != nil {
		return HubTokenTimes{}, errTokenShape
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return HubTokenTimes{}, errTokenShape
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return HubTokenTimes{}, errTokenShape
	}
	return HubTokenTimes{Expiry: time.Unix(exp, 0).UTC(), Now: time.Unix(now, 0).UTC()}, nil
}

// HarnessAlive reports whether the agent's harness is still running: the
// pane of scion's `agent` tmux window is alive. scion starts the harness as
// `sh -c 'claude …; echo $? > /tmp/scion-harness-exit-code'` in that window
// (the `shell` window keeps the session itself alive), so the window goes
// away when claude exits. tmux answering "no such window" or "no server" is
// a false; anything else that fails is an error, never a false.
func (p AgentProbe) HarnessAlive(ctx context.Context, ref string) (bool, error) {
	if err := checkRef(ref); err != nil {
		return false, fmt.Errorf("probing agent harness: %w", err)
	}
	res, err := p.R.Run(ctx, nil, "podman", "exec", ref, "tmux", "list-panes", "-t", "scion:agent", "-F", "#{pane_dead}")
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
	case noSuchContainer(res.Stderr):
		return false, fmt.Errorf("probing agent harness in %s: %w", ref, ErrNoContainer)
	case strings.Contains(s, "can't find window"), strings.Contains(s, "can't find session"),
		strings.Contains(s, "no server running"), strings.Contains(s, "error connecting to"):
		return false, nil
	}
	return false, fmt.Errorf("probing agent harness in %s: exit status %d", ref, res.Code)
}

// ReportSessionRunning makes the agent report its own session running to the
// hub, exactly as Claude Code's SessionStart hook does: `sciontool hook
// SessionStart` in the container, as the container's user, with the agent's
// own hub token (scion pkg/sciontool/hooks/handlers/hub.go reports phase
// running, activity working). Nothing new enters the container. sciontool
// logs a failed hub call and still exits 0, so the caller re-reads the phase
// to learn whether it worked.
func (p AgentProbe) ReportSessionRunning(ctx context.Context, ref string) error {
	if err := checkRef(ref); err != nil {
		return fmt.Errorf("reporting agent session: %w", err)
	}
	res, err := p.R.Run(ctx, nil, "podman", "exec", ref, "sciontool", "hook", "--dialect=claude", "SessionStart")
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

// noSuchContainer reports podman's answer for a container it does not know.
func noSuchContainer(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object")
}
