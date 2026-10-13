package jail

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// The controller PAT and a hub assertion never ride the host command line:
// every in-jail command is a host process whose argv any local host user can
// read with `ps`. Neither prefix binary is relied on to forward the host
// environment (ssh-backed prefixes do not by default), so the host argv
// carries only a fixed script and the secret goes one of two guest-side ways:
//
//   - Run/RunIn/RunStdin: the secret is the first line of the child's stdin,
//     ahead of any payload. A POSIX sh reads a pipe one byte at a time for
//     `read`, so the payload behind it reaches the command untouched.
//   - AttachArgv (PAT only): stdin is the user's terminal, so StageHubToken
//     writes a 0600 file in the run user's XDG_RUNTIME_DIR (a 0700 tmpfs),
//     which hubTokenReadScript reads and removes before the exec.
//
// Both scripts refuse to run without XDG_RUNTIME_DIR rather than fall back
// to a shared path.

const hubTokenEnv = "SCION_HUB_TOKEN"

// HubAssertionEnv carries a signed hub assertion into a jailed command. It is
// as sensitive as the PAT while it lives, so the runner moves it to stdin the
// same way.
const HubAssertionEnv = "LEVER_HUB_ASSERTION"

// hubTokenStdinScript: `IFS=` and -r keep the value byte-exact; a missing
// line is an error, never an empty token.
const hubTokenStdinScript = `IFS= read -r t || exit 1; export ` + hubTokenEnv + `="$t"; exec "$@"`

const hubAssertionStdinScript = `IFS= read -r t || exit 1; export ` + HubAssertionEnv + `="$t"; exec "$@"`

// stdinSecretScripts maps every env key the runner withholds from the argv to
// the guest script that re-exports it from the first stdin line.
var stdinSecretScripts = map[string]string{
	hubTokenEnv:     hubTokenStdinScript,
	HubAssertionEnv: hubAssertionStdinScript,
}

const hubTokenFile = "$XDG_RUNTIME_DIR/lever-hub.pat"

// stageHubTokenScript removes the old file first so the write never follows
// a planted link and the mode is set by umask, not inherited.
const stageHubTokenScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; umask 077; rm -f "` + hubTokenFile + `" && cat > "` + hubTokenFile + `"`

// hubTokenReadScript removes the file before the exec, so the PAT is not at
// rest in the guest for the session: the attach is a syscall.Exec on the
// host, so nothing host-side could clean up afterwards. `$(…)` strips
// trailing newlines only, which the token never carries.
const hubTokenReadScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; t=$(cat "` + hubTokenFile + `") || exit 1; rm -f "` + hubTokenFile + `"; export ` + hubTokenEnv + `="$t"; exec "$@"`

// checkHubToken refuses a token the one-line transport could only truncate.
func checkHubToken(tok string) error {
	if strings.ContainsAny(tok, "\r\n") {
		return fmt.Errorf("hub token contains a line break; it cannot be passed to the jail")
	}
	return nil
}

// stdinSecret picks the one withheld key env carries. Two would need two
// stdin lines and an order both sides agree on; no caller needs that, so it
// is refused.
func stdinSecret(env map[string]string) (key, val string, err error) {
	_, hasTok := env[hubTokenEnv]
	_, hasAssert := env[HubAssertionEnv]
	if hasTok && hasAssert {
		return "", "", fmt.Errorf("both %s and %s are set; a jailed command takes at most one", hubTokenEnv, HubAssertionEnv)
	}
	if hasAssert {
		val = env[HubAssertionEnv]
		if strings.ContainsAny(val, "\r\n") {
			return "", "", fmt.Errorf("hub assertion contains a line break; it cannot be passed to the jail")
		}
		return HubAssertionEnv, val, nil
	}
	val = env[hubTokenEnv]
	if err := checkHubToken(val); err != nil {
		return "", "", err
	}
	return hubTokenEnv, val, nil
}

// StageHubToken writes tok (mode 0600) to the run user's runtime directory
// inside the jail for WithHubTokenFromFile; the token travels on stdin. Call
// it right before every attach: the read script removes the file, so an
// attach without a fresh stage fails rather than run token-less.
func StageHubToken(ctx context.Context, r proc.Runner, tok string) error {
	if tok == "" {
		return fmt.Errorf("staging hub token: empty token")
	}
	if err := checkHubToken(tok); err != nil {
		return fmt.Errorf("staging hub token: %w", err)
	}
	res, err := r.RunStdin(ctx, strings.NewReader(tok), nil, "sh", "-c", stageHubTokenScript)
	if err != nil {
		return fmt.Errorf("staging hub token in the jail: %w: %s", err, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// WithHubTokenFromFile wraps an in-jail interactive command so the guest
// shell exports the token StageHubToken staged, then execs inner unchanged.
func WithHubTokenFromFile(inner []string) []string {
	return append([]string{"sh", "-c", hubTokenReadScript, "_"}, inner...)
}
