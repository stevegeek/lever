package guest

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/provision/loginfwd"
)

const (
	LoginForwardPath = "/usr/local/bin/lever-login-forward"

	// loginForwardLog carries connection-level failures only; no request
	// ever passes through it in readable form.
	loginForwardLog = "/tmp/lever-login-forward.log"
)

// disableLoginForwardScript runs through the ROOT prefix: root can signal a
// process the run user owns. Bare command names are safe here because they
// resolve on root's PATH, which no run user can write to.
//
// pkill exit 1 means nothing matched; anything above means the kill did not
// happen, and removing the binary while its process keeps bridging is
// exactly the state this exists to prevent.
const disableLoginForwardScript = `set -u
if [ ! -e ` + LoginForwardPath + ` ]; then echo "FOUND 0"; exit 0; fi
pkill -f '` + loginForwardMatch + `'
rc=$?
if [ $rc -gt 1 ]; then echo "could not stop the login forwarder (pkill exit $rc)" >&2; exit 1; fi
rm -f ` + LoginForwardPath + `
echo "FOUND 1"
`

func (g Guest) ensureLoginForwarder(ctx context.Context, spec types.HubLogin) error {
	arch, err := g.GOARCH(ctx)
	if err != nil {
		return fmt.Errorf("guest: detect guest architecture: %w", err)
	}
	bin, _, err := loginfwd.Forwarder(ctx, g.Host, arch, g.Machine)
	if err != nil {
		return fmt.Errorf("guest: %w", err)
	}
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

// loginForwardMatch matches ANY login forwarder, whatever arguments it
// carries; anchored, so it cannot match a script that merely mentions the
// path. Matching the path is load-bearing on the stop side: a forwarder with
// a stale `-target` still holds the listen port, so the replacement could not
// bind.
const loginForwardMatch = "^" + LoginForwardPath

// loginForwardScript decides on the ARGUMENTS, not merely on whether
// something is running:
//
//   - Skip only when a process with EXACTLY this argv is running AND the port
//     answers, so a stale `-target` gets replaced.
//   - Stop by binary path, whatever argv the running one carries.
//   - Succeed only when a process with THIS argv is running and the port
//     answers: a stale process can answer the port while the replacement
//     died unable to bind.
//
// force is set when the binary was just replaced, so the old code stops
// being served.
//
// Absolute paths throughout: a bare command name resolves on the run user's
// PATH, which has run-user-writable directories ahead of /usr/bin. The
// liveness probe uses bash's builtin /dev/tcp, so there is nothing to shadow.
func loginForwardScript(spec types.HubLogin, force bool) string {
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
