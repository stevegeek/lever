package jail

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// The controller PAT never rides the host command line.
//
// Every in-jail command is a host process — `orb …` or `limactl shell …` —
// whose full argv any local host user can read with `ps`. An `env
// SCION_HUB_TOKEN=<pat>` word in that argv therefore published the PAT for as
// long as the command ran, and for the whole session on the attach path.
// Neither prefix binary is relied on to forward the host process environment
// into the guest (nothing in lever verifies that either does, and ssh-backed
// prefixes do not by default), so the token takes one of two guest-side
// routes, and the host argv carries only a fixed script:
//
//   - Non-interactive commands (Run/RunIn/RunStdin): the token is the FIRST
//     LINE of the child's stdin, ahead of any payload. hubTokenStdinScript
//     reads that one line in the guest, exports it and execs the real
//     command. A POSIX sh reads a pipe one byte at a time for `read`, so a
//     payload behind the line (an inline config for `--config -`, an image
//     archive) reaches the command untouched.
//   - Interactive commands (AttachArgv): stdin is the user's terminal, so
//     StageHubToken first writes the token to a 0600 file in the run user's
//     XDG_RUNTIME_DIR (a 0700 tmpfs owned by that user, emptied on reboot),
//     and hubTokenReadScript reads it back into the environment before exec.
//
// Both scripts refuse to run without XDG_RUNTIME_DIR rather than fall back
// to a shared path; jailEnv sets it on every in-jail command.

// hubTokenEnv is the one env key the runner keeps off the host argv.
const hubTokenEnv = "SCION_HUB_TOKEN"

// hubTokenStdinScript reads the token line from stdin, exports it and execs
// the positionals. `IFS=` and -r keep the value byte-exact; a missing line
// is an error, never an empty token.
const hubTokenStdinScript = `IFS= read -r t || exit 1; export ` + hubTokenEnv + `="$t"; exec "$@"`

// hubTokenFile is the staged token's guest path, expanded by the guest shell.
const hubTokenFile = "$XDG_RUNTIME_DIR/lever-hub.pat"

// stageHubTokenScript writes stdin to hubTokenFile as a fresh 0600 file.
// The old file is removed first so the write never follows a planted link
// and the mode is set by umask, not inherited.
const stageHubTokenScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; umask 077; rm -f "` + hubTokenFile + `" && cat > "` + hubTokenFile + `"`

// hubTokenReadScript exports the staged token and execs the positionals.
// `$(…)` strips trailing newlines only, which the token never carries.
const hubTokenReadScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; t=$(cat "` + hubTokenFile + `") || exit 1; export ` + hubTokenEnv + `="$t"; exec "$@"`

// checkHubToken refuses a token the one-line transport could only truncate.
func checkHubToken(tok string) error {
	if strings.ContainsAny(tok, "\r\n") {
		return fmt.Errorf("hub token contains a line break; it cannot be passed to the jail")
	}
	return nil
}

// StageHubToken writes tok to the run user's runtime directory inside the
// jail (mode 0600) through r, the jail runner, so an interactive command
// wrapped by WithHubTokenFromFile can read it there. The token travels on
// stdin; the host argv carries only the fixed staging script. Call it right
// before the attach so a re-minted PAT is what the session uses.
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
// Every positional after the script is the inner command, so nothing in
// inner can be read as an argument of the wrapper.
func WithHubTokenFromFile(inner []string) []string {
	return append([]string{"sh", "-c", hubTokenReadScript, "_"}, inner...)
}
