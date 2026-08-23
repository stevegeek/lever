package guest

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/provision/loginfwd"
)

// Installing, starting and stopping the guest-side login forwarder. See
// hublogin.go for why it exists.

const (
	// LoginForwardPath is where the guest-side forwarder is installed. Beside
	// scionDestPath, for the same reason: it is a part of scion's environment
	// that lever puts there.
	LoginForwardPath = "/usr/local/bin/lever-login-forward"

	// loginForwardLog is where the forwarder's own stderr goes in the guest.
	// It carries connection-level failures only; no request ever passes
	// through it in readable form.
	loginForwardLog = "/tmp/lever-login-forward.log"
)

// disableLoginForwardScript kills and removes the guest-side forwarder,
// reporting whether there was one. Run through the ROOT prefix: the binary
// lives in /usr/local/bin, and root can signal a process the run user owns.
//
// pkill's exit codes are read rather than swallowed: 1 means "nothing matched"
// (a binary installed but not running, which is fine), while anything above
// that — 127 for a pkill that is not there at all — means the kill did not
// happen, and removing the binary while its process keeps bridging would be
// exactly the state this exists to prevent.
//
// Bare command names here, unlike loginForwardScript: these resolve on
// guest ROOT's PATH, which no run user can write to, and that is the
// convention InstallRootBinary and stageWebAssetsScript already follow.
const disableLoginForwardScript = `set -u
if [ ! -e ` + LoginForwardPath + ` ]; then echo "FOUND 0"; exit 0; fi
pkill -f '` + loginForwardMatch + `'
rc=$?
if [ $rc -gt 1 ]; then echo "could not stop the login forwarder (pkill exit $rc)" >&2; exit 1; fi
rm -f ` + LoginForwardPath + `
echo "FOUND 1"
`

// ensureLoginForwarder builds the forwarder for the guest's architecture,
// installs it if the guest does not already hold those exact bytes, and makes
// sure it is running with the arguments this spec asks for.
func (g Guest) ensureLoginForwarder(ctx context.Context, spec HubLogin) error {
	arch, err := g.GOARCH(ctx)
	if err != nil {
		return fmt.Errorf("guest: detect guest architecture: %w", err)
	}
	bin, err := loginfwd.Build(ctx, g.Host, arch, g.Machine)
	if err != nil {
		return fmt.Errorf("guest: %w", err)
	}
	// Same hash-skip as the scion binary: compare against the file in the
	// guest, so a deleted or replaced binary re-installs rather than being
	// assumed present.
	replaced, err := g.InstallRootBinaryIfChanged(ctx, bin, LoginForwardPath)
	if err != nil {
		return fmt.Errorf("guest: %w", err)
	}
	// A freshly installed binary must displace whatever is running, or the
	// guest keeps serving the old code from the old process.
	if _, err := g.UserRun(ctx, "/bin/bash", "-c", loginForwardScript(spec, replaced)); err != nil {
		return fmt.Errorf("guest: start the login forwarder on 127.0.0.1:%d: %w", spec.IssuerPort, err)
	}
	return nil
}

// loginForwardMatch is the pgrep/pkill pattern for ANY login forwarder,
// whatever arguments it carries. Anchored, so it cannot match the script that
// merely mentions the path.
//
// Matching the PATH rather than the desired argv is load-bearing on the stop
// side. A forwarder whose `-target` port changed — an operator editing
// remote.login_port does exactly that — is still holding the listen port, so
// killing only an exact-argv match leaves it running, the replacement unable
// to bind, and the port answering all the same. That was a live failure.
const loginForwardMatch = "^" + LoginForwardPath

// loginForwardScript is the exact bash body that makes the forwarder run with
// the arguments spec asks for. Split out from ensureLoginForwarder so a test
// can assert it without a guest (the shape scionConfigRemoveScript uses).
//
// Every decision it makes is about the ARGUMENTS, not merely about whether
// something is running:
//
//   - Skip only when a process with EXACTLY this argv is running AND the port
//     answers. A forwarder carrying a stale `-target` (an operator edited
//     remote.login_port) fails that test and gets replaced.
//   - Stop by BINARY PATH, whatever argv the running one carries. Killing only
//     an exact-argv match left a stale forwarder holding the listen port,
//     which is what made this a live failure rather than a no-op.
//   - Declare success only when a process with THIS argv is running and the
//     port answers. "The port answers" alone was satisfied by the stale
//     process, so the script reported success while the replacement had
//     already died unable to bind — the silent half of the same bug.
//
// `force` is set when the binary was just replaced, since a process running
// the right arguments would otherwise go on serving the old code.
//
// Absolute paths throughout: UserRun passes no env, so a bare command name
// resolves on the guest run-user's PATH, which has run-user-writable
// directories ahead of /usr/bin. bash's own /dev/tcp is used for the liveness
// probe precisely because it is a builtin — there is no netcat to shadow.
func loginForwardScript(spec HubLogin, force bool) string {
	argv := fmt.Sprintf("%s -listen 127.0.0.1:%d -target %s:%d",
		LoginForwardPath, spec.IssuerPort, spec.HostAddress, spec.HostPort)
	listening := fmt.Sprintf("(exec 3<>/dev/tcp/127.0.0.1/%d) 2>/dev/null", spec.IssuerPort)
	restart := "false"
	if force {
		restart = "true"
	}
	return fmt.Sprintf(`set -u
want=%s
force=%s
if [ "$force" != "true" ] && /usr/bin/pgrep -f -x "$want" >/dev/null 2>&1 && %s; then
  exit 0
fi
/usr/bin/pkill -f %s >/dev/null 2>&1
rc=$?
if [ $rc -gt 1 ]; then echo "could not stop the running login forwarder (pkill exit $rc)" >&2; exit 1; fi
for _ in $(/usr/bin/seq 1 30); do
  /usr/bin/pgrep -f %s >/dev/null 2>&1 || break
  /usr/bin/sleep 0.1
done
/usr/bin/setsid /usr/bin/nohup $want >>%s 2>&1 &
for _ in $(/usr/bin/seq 1 50); do
  if /usr/bin/pgrep -f -x "$want" >/dev/null 2>&1 && %s; then exit 0; fi
  /usr/bin/sleep 0.1
done
echo "the login forwarder is not running with the arguments this apply asked for; see %s in the jail" >&2
exit 1
`, shellSingleQuote(argv), restart, listening,
		shellSingleQuote(loginForwardMatch), shellSingleQuote(loginForwardMatch),
		shellSingleQuote(loginForwardLog), listening, loginForwardLog)
}
