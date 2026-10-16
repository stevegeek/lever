package jail

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// The controller PAT never rides the host command line: every in-jail
// command is a host process whose argv any local host user can read with
// `ps`. Neither prefix binary is relied on to forward the host environment
// (ssh-backed prefixes do not by default), so the host argv carries only a
// fixed script and the token goes one of two guest-side ways:
//
//   - Run/RunIn/RunStdin: the token is the first line of the child's stdin,
//     ahead of any payload. A POSIX sh reads a pipe one byte at a time for
//     `read`, so the payload behind it reaches the command untouched.
//   - AttachArgv: stdin is the user's terminal, so StageHubToken writes a
//     0600 file in the run user's XDG_RUNTIME_DIR (a 0700 tmpfs), which
//     hubTokenReadScript reads and removes before the exec.
//
// Both scripts refuse to run without XDG_RUNTIME_DIR rather than fall back
// to a shared path.

const hubTokenEnv = "SCION_HUB_TOKEN"

// hubTokenStdinScript: `IFS=` and -r keep the value byte-exact; a missing
// line is an error, never an empty token.
const hubTokenStdinScript = `IFS= read -r t || exit 1; export ` + hubTokenEnv + `="$t"; exec "$@"`

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

// WithHubTokenFromStdin wraps an in-jail streaming command so the guest shell
// reads the token from the first line of its stdin, exports it, then execs
// inner with the rest of the stream: the caller writes the token line ahead
// of the payload.
func WithHubTokenFromStdin(inner []string) []string {
	return append([]string{"sh", "-c", hubTokenStdinScript, "_"}, inner...)
}

// HubTokenLine is the stdin prefix WithHubTokenFromStdin reads.
func HubTokenLine(tok string) (string, error) {
	if tok == "" {
		return "", fmt.Errorf("hub token line: empty token")
	}
	if err := checkHubToken(tok); err != nil {
		return "", err
	}
	return tok + "\n", nil
}
