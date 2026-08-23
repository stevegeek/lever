package guest

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"gopkg.in/yaml.v3"
)

// The guest half of lever's remote-access login path.
//
// scion's web UI is opened by a session cookie, which lever obtains by driving
// scion's OIDC login server-side from the host proxy (internal/remoteproxy).
// Two facts decide the shape of everything below:
//
//   - The hub validates the issuer URL AT STARTUP and refuses to start unless
//     it is https, or http on localhost/127.0.0.1 (pkg/hub/oauth.go
//     validateOIDCLoginConfig). So the hub can only be pointed at a provider
//     it believes is local.
//   - The provider must run host-side, because authorization codes must be
//     mintable ONLY by an in-process call inside the proxy. Splitting it —
//     HTTP surface in the guest, minting on the host — would put a minted code
//     on a channel crossing into the jail, which is the entire problem being
//     avoided.
//
// So: a logic-free forwarder listens on the guest's loopback at the provider's
// port and carries bytes to the host (loginfwd/main.go), and the hub's
// oidc_login block names http://127.0.0.1:<port> as the issuer. The hub reads
// that block once, at startup, so EnsureHubLogin reports whether it changed
// and the caller restarts the hub when it did.

//go:embed loginfwd/main.go
var loginForwardSource string

const (
	// LoginForwardPath is where the guest-side forwarder is installed. Beside
	// scionDestPath, for the same reason: it is a part of scion's environment
	// that lever puts there.
	LoginForwardPath = "/usr/local/bin/lever-login-forward"

	// scionSettingsRel and scionServerYAMLRel are scion's machine-level
	// config files, relative to the guest run user's home.
	scionSettingsRel   = ".scion/settings.yaml"
	scionServerYAMLRel = ".scion/server.yaml"

	// loginForwardLog is where the forwarder's own stderr goes in the guest.
	// It carries connection-level failures only; no request ever passes
	// through it in readable form.
	loginForwardLog = "/tmp/lever-login-forward.log"

	// loginDisplayName is what scion's login page calls the provider.
	loginDisplayName = "Lever remote access"

	// operatorDisplayName names an operator in the hub's `server.auth` block.
	//
	// It exists to clear scion's first-run wizard, not to assert who is
	// connected. The SPA redirects EVERY fresh load to /onboarding until
	// `GET /api/v1/system/status` reports complete, and that is
	// `Initialized && IdentitySet && RuntimeOK && HarnessesSeeded`
	// (pkg/hub/system_handlers.go). IdentitySet is true only when
	// `server.auth` carries a display_name, email or username — so a hub lever
	// has fully set up still greets the operator with a setup wizard, on every
	// load, because nothing ever named a user in that file.
	//
	// Writing it is inert: those three fields reach scion only as
	// hub.DevUserConfig, which it reads under `if cfg.DevAuthToken != ""`
	// (pkg/hub/server.go seedDevUser, and DevAuthMiddleware). lever runs
	// --dev-auth=false, so this names the wizard's user and nothing else. It is
	// deliberately NOT an operator's email: with remote.allowed_users set, each
	// operator gets their own hub user keyed on their tailnet login
	// (remoteproxy identityFor), and this field must not compete with that.
	operatorDisplayName = "Lever"
)

// EnsureHubLogin puts the guest into the state lever's remote login needs:
// the forwarder built, installed and listening, and the hub's oidc_login block
// present in ~/.scion/settings.yaml.
//
// It reports whether the hub's configuration CHANGED. scion reads that file
// once, at startup, so a change means a running hub is still serving the old
// configuration and the caller must restart it. An unchanged config restarts
// nothing, which is what keeps a re-apply from bouncing the hub (and every
// agent's connection to it) for no reason.
func (g Guest) EnsureHubLogin(ctx context.Context, spec backend.HubLogin) (bool, error) {
	if spec.IssuerPort <= 0 || spec.HostPort <= 0 || spec.HostAddress == "" || spec.ClientID == "" {
		return false, fmt.Errorf("guest: hub login: issuer port, host port, host address and client id are all required")
	}
	if spec.IssuerPort == spec.HostPort {
		// OrbStack mirrors the guest listener onto the host at the same
		// number, so one number for both halves means the provider cannot
		// bind its own port. See backend.GuestLoginIssuerPort.
		return false, fmt.Errorf("guest: hub login: the guest issuer port and the host port must differ (both are %d)", spec.HostPort)
	}
	// The forwarder first: once the hub restarts with oidc_login set, its very
	// first login attempt dials the issuer, and finding nothing there caches
	// a broken discovery for the length of one request rather than working.
	if err := g.ensureLoginForwarder(ctx, spec); err != nil {
		return false, err
	}
	return g.ensureHubLoginSettings(ctx, spec)
}

// DisableHubLogin converges the guest's login path OFF: it stops the
// forwarder, removes it, and drops the `oidc_login` block from the hub's
// settings.
//
// The forwarder is the part that matters. It is an unauthenticated TCP bridge
// from guest loopback to a host loopback port, reachable from every agent's
// network namespace, and it outlives the feature it was installed for: with
// remote access turned back off, the host-side provider is gone but a port
// beside lever's own 8443/8444/8445 stays bridged into the jail, so any FUTURE
// host listener that lands there would be reachable from inside it. Stopping
// the proxy is not enough; the bridge has to go too.
//
// It reports whether it removed HUB CONFIGURATION — the `oidc_login` block, or
// the `display_name` lever wrote beside it. That is the OFF path's half of the
// signal EnsureHubLogin gives the ON path, and it is there for the same reason:
// the hub reads that file once, at startup, so a hub that is already running
// was started FROM the file this just edited and goes on advertising a login
// whose provider has gone. Only a restart replaces that, and only the caller
// can order one (internal/apply.Run).
//
// The forwarder is deliberately NOT part of that answer. No hub ever reads it,
// and a restart drops every agent's connection to the hub — so the signal has
// to mean "the hub is serving something this apply took away", not merely
// "something was removed".
//
// Every apply of every instance with remote access off pays for both halves, so
// both have to be quiet when there is nothing to do: the guest script exits
// early on a missing binary, and hubSettingsWithoutLogin reports
// (unchanged, false) for every "nothing there" shape. A converged instance
// costs two round trips, writes nothing, and reports false.
func (g Guest) DisableHubLogin(ctx context.Context) (bool, error) {
	if _, err := g.RootRun(ctx, "bash", "-c", disableLoginForwardScript); err != nil {
		return false, fmt.Errorf("guest: stop the login forwarder: %w", err)
	}
	// The settings edit is NOT gated on having found the binary. It used to be,
	// and that made the failure permanent: the script `rm -f`s the binary
	// BEFORE the settings edit runs, so if that edit ever failed (a transient
	// guest error, the machine going away mid-command), the next apply found no
	// binary, returned early, and the `oidc_login` block stayed in the guest
	// forever — advertising a login for a provider that no longer runs, with no
	// lever verb that removes it. Convergence must not depend on a step that
	// already succeeded. removeHubLoginSettings is itself a quiet no-op when
	// there is nothing to remove (see hubSettingsWithoutLogin).
	return g.removeHubLoginSettings(ctx)
}

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
// Bare command names here, unlike the run-user script above: these resolve on
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

// removeHubLoginSettings drops the oidc_login block from the guest's settings,
// and reports whether it wrote the file.
//
// Fail-soft on a file it cannot parse or read: by the time this runs the
// forwarder is already gone, so what is left is a block naming a dead
// provider — not worth failing an apply whose only instruction was to turn
// remote access off. Each of those paths reports "no change", which is the
// honest answer: nothing was removed, so nothing the hub is serving changed.
// A failed WRITE is different, and stays fatal — the file was there, lever
// meant to edit it, and reporting no change would tell the caller the guest is
// converged when it is not.
func (g Guest) removeHubLoginSettings(ctx context.Context) (bool, error) {
	res, err := g.UserRun(ctx, "/bin/bash", "-c", readScionSettingsScript)
	if err != nil {
		return false, nil
	}
	existing, _, err := parseScionSettingsRead(res.Stdout)
	if err != nil {
		return false, nil
	}
	updated, changed, err := hubSettingsWithoutLogin(existing)
	if err != nil || !changed {
		return false, nil
	}
	if err := g.writeScionSettings(ctx, updated); err != nil {
		return false, err
	}
	return true, nil
}

// ensureLoginForwarder builds the forwarder for the guest's architecture,
// installs it if the guest does not already hold those exact bytes, and makes
// sure it is running with the arguments this spec asks for.
func (g Guest) ensureLoginForwarder(ctx context.Context, spec backend.HubLogin) error {
	bin, err := g.buildLoginForwarder(ctx)
	if err != nil {
		return err
	}
	// Same hash-skip as the scion binary: compare against the file in the
	// guest, so a deleted or replaced binary re-installs rather than being
	// assumed present.
	want, err := hashFile(bin)
	if err != nil {
		return fmt.Errorf("guest: hashing %s: %w", bin, err)
	}
	replaced := true
	// Absolute path, not a bare name: UserRun passes no env, so a bare name
	// resolves on the guest run-user's PATH, which precedes /usr/bin with
	// run-user-writable directories (see InstallRootBinaryIfChanged).
	if res, err := g.UserRun(ctx, "/usr/bin/sha256sum", LoginForwardPath); err == nil {
		if f := strings.Fields(res.Stdout); len(f) > 0 && f[0] == want {
			replaced = false
		}
	}
	if replaced {
		if err := g.InstallRootBinary(ctx, bin, LoginForwardPath); err != nil {
			return err
		}
	}
	// A freshly installed binary must displace whatever is running, or the
	// guest keeps serving the old code from the old process.
	if _, err := g.UserRun(ctx, "/bin/bash", "-c", loginForwardScript(spec, replaced)); err != nil {
		return fmt.Errorf("guest: start the login forwarder on 127.0.0.1:%d: %w", spec.IssuerPort, err)
	}
	return nil
}

// buildLoginForwarder cross-compiles the forwarder for the guest, from a copy
// of its own source.
//
// The source is embedded rather than built from lever's checkout because lever
// is normally an installed BINARY with no source tree beside it — while the
// guest still needs a linux executable for its own architecture. The program
// is stdlib-only, so the build needs a Go toolchain and nothing else: no
// module download, no network, no dependency on where lever came from.
func (g Guest) buildLoginForwarder(ctx context.Context) (string, error) {
	arch, err := g.GOARCH(ctx)
	if err != nil {
		return "", fmt.Errorf("guest: detect guest architecture: %w", err)
	}
	dir := filepath.Join(os.TempDir(), "lever-loginfwd-"+g.Machine)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("guest: login forwarder build dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(loginForwardSource), 0o600); err != nil {
		return "", fmt.Errorf("guest: stage login forwarder source: %w", err)
	}
	// A module of its own: the source is compiled outside lever's module, and
	// go build refuses to work without one. Nothing is ever downloaded for it.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(loginForwardGoMod), 0o600); err != nil {
		return "", fmt.Errorf("guest: stage login forwarder go.mod: %w", err)
	}
	goBin, err := g.goBinary(ctx)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "lever-login-forward")
	// -trimpath keeps the output independent of the build directory, so the
	// hash-skip above compares like with like across hosts and runs.
	if _, err := g.Host.RunIn(ctx, dir, map[string]string{"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0", "GOFLAGS": "-mod=mod", "GOPROXY": "off"},
		goBin, "build", "-trimpath", "-o", out, "."); err != nil {
		return "", fmt.Errorf("guest: cross-compile the login forwarder (remote access needs a Go toolchain on this host): %w", err)
	}
	return out, nil
}

// loginForwardGoMod is the module file the embedded source is built with. The
// Go directive is deliberately conservative: the program uses nothing newer,
// and a floor above the host's toolchain would fail the build for no reason.
const loginForwardGoMod = "module lever-login-forward\n\ngo 1.22\n"

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
func loginForwardScript(spec backend.HubLogin, force bool) string {
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

// goBinary resolves the REAL go binary (GOROOT/bin/go) rather than trusting
// `go` on PATH from an arbitrary working directory. Same reasoning as
// fetchScionModule: a version manager that resolves `go` by walking up for a
// project file (asdf, mise) cannot resolve it from a directory outside any
// project — which is exactly where both the module cache and this build live.
func (g Guest) goBinary(ctx context.Context) (string, error) {
	root, err := g.Host.Run(ctx, nil, "go", "env", "GOROOT")
	if err != nil {
		return "", fmt.Errorf("resolve go toolchain (is go on PATH?): %w", err)
	}
	return filepath.Join(strings.TrimSpace(root.Stdout), "bin", "go"), nil
}

// ensureHubLoginSettings writes the oidc_login block into the guest's
// ~/.scion/settings.yaml, and reports whether the file changed.
func (g Guest) ensureHubLoginSettings(ctx context.Context, spec backend.HubLogin) (bool, error) {
	res, err := g.UserRun(ctx, "/bin/bash", "-c", readScionSettingsScript)
	if err != nil {
		// Deliberately fatal rather than "assume empty": treating an
		// unreadable settings file as absent would rewrite scion's machine
		// configuration from nothing.
		return false, fmt.Errorf("guest: read %s: %w", scionSettingsRel, err)
	}
	existing, hasServerYAML, err := parseScionSettingsRead(res.Stdout)
	if err != nil {
		return false, err
	}
	updated, changed, err := hubSettingsConverged(existing, spec, hasServerYAML)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := g.writeScionSettings(ctx, updated); err != nil {
		return false, err
	}
	return true, nil
}

// writeScionSettings streams new settings content over the guest's file, as
// the RUN USER (it is that user's own config, not root's). Same transport and
// atomicity as InstallRootBinary: the content is the stdin of a guest-side
// script that writes a temp file and mv's it over the destination.
func (g Guest) writeScionSettings(ctx context.Context, content []byte) error {
	if err := g.pipeInto(ctx, g.UserPrefix, bytes.NewReader(content), writeScionSettingsScript); err != nil {
		return fmt.Errorf("guest: write %s: %w", scionSettingsRel, err)
	}
	return nil
}

// readScionSettingsScript prints the machine-level settings file, preceded by
// a line saying whether the legacy server.yaml exists beside it. Both answers
// come from one round trip, and an absent settings.yaml is reported as empty
// content rather than as a failure.
var readScionSettingsScript = fmt.Sprintf(`set -u
if [ -f "$HOME/%s" ]; then echo "LEGACY 1"; else echo "LEGACY 0"; fi
if [ -f "$HOME/%s" ]; then cat "$HOME/%s"; fi
`, scionServerYAMLRel, scionSettingsRel, scionSettingsRel)

// parseScionSettingsRead splits readScionSettingsScript's output into the
// settings file's content and the legacy-server.yaml answer.
func parseScionSettingsRead(out string) (settings []byte, hasServerYAML bool, err error) {
	line, rest, ok := strings.Cut(out, "\n")
	if !ok || !strings.HasPrefix(line, "LEGACY ") {
		return nil, false, fmt.Errorf("guest: could not read the jail's scion settings (unexpected output)")
	}
	return []byte(rest), strings.TrimSpace(strings.TrimPrefix(line, "LEGACY ")) == "1", nil
}

// writeScionSettingsScript is the guest-side half of writeScionSettings: read
// stdin into a temp file beside the settings file, then swap it in.
var writeScionSettingsScript = fmt.Sprintf(`mkdir -p "$HOME/%s" && cat > "$HOME/%s.lever-tmp" && mv "$HOME/%s.lever-tmp" "$HOME/%s"`,
	filepath.Dir(scionSettingsRel), scionSettingsRel, scionSettingsRel, scionSettingsRel)

// v1OIDCLogin is scion's oidc_login block as it appears under `server:` in
// settings.yaml (pkg/config/settings_v1.go V1OIDCLoginConfig).
//
// Four keys, and deliberately not the other two. client_secret is omitted
// because there is no secret: lever's provider is a public client and scion
// only sends the parameter when it is non-empty. scopes is omitted because
// scion's default is already exactly "openid email profile" (pkg/hub/
// oauth.go oidcScopes), and a config file should not restate a default.
type v1OIDCLogin struct {
	Enabled     bool   `yaml:"enabled"`
	DisplayName string `yaml:"display_name"`
	IssuerURL   string `yaml:"issuer_url"`
	ClientID    string `yaml:"client_id"`
}

// hubSettingsConverged returns the settings file content lever wants for a hub
// with remote login ON, and whether it differs from what is already there. It
// converges THREE things under `server:`, each restart-worthy on its own
// because scion reads the whole file once at startup:
//
//   - `oidc_login`: lever's login block (v1OIDCLogin), replaced whenever it
//     differs from what this spec wants.
//   - `auth.display_name`: an operator name, added only when nothing names one
//     (setOperatorDisplayName) — it clears scion's first-run wizard.
//   - `message_broker.enabled: true`, added only when the key is absent
//     (enableMessageBroker) — it makes native chat work.
//
// hubSettingsWithoutLogin is the OFF path; see it for which of these it
// reverts and why.
//
// It edits the parsed document rather than rewriting the file, so every other
// key — and every comment — survives. "Changed" is decided semantically, by
// comparing the block that is there against the block lever wants: a
// re-serialisation that only moves whitespace must not read as a change, or
// every apply would restart the hub.
func hubSettingsConverged(existing []byte, spec backend.HubLogin, hasServerYAML bool) ([]byte, bool, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := yaml.Unmarshal(existing, &doc); err != nil {
			return nil, false, fmt.Errorf("guest: parse the jail's %s: %w", scionSettingsRel, err)
		}
	}
	root := documentRoot(&doc)
	if root == nil {
		return nil, false, fmt.Errorf("guest: the jail's %s is not a YAML mapping", scionSettingsRel)
	}
	server := mapGet(root, "server")
	if server == nil && hasServerYAML {
		// Adding a `server` key here would silently move scion's whole server
		// configuration from the legacy server.yaml to this file, because
		// settings.yaml wins outright once it has that key (pkg/config/
		// hub_config.go loadGlobalConfigFromSettings). Whatever server.yaml
		// holds would stop being read. Refuse rather than guess.
		return nil, false, fmt.Errorf("guest: the jail has a legacy ~/%s and no `server:` key in ~/%s — "+
			"adding one would make scion ignore server.yaml entirely. Consolidate them first "+
			"(`scion config migrate --server` in the jail), then re-run `lever apply`",
			scionServerYAMLRel, scionSettingsRel)
	}
	if server == nil {
		server = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapSet(root, "server", server)
	}
	if server.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("guest: `server:` in the jail's %s is not a mapping", scionSettingsRel)
	}

	var want yaml.Node
	if err := want.Encode(v1OIDCLogin{
		Enabled:     true,
		DisplayName: loginDisplayName,
		IssuerURL:   fmt.Sprintf("http://127.0.0.1:%d", spec.IssuerPort),
		ClientID:    spec.ClientID,
	}); err != nil {
		return nil, false, fmt.Errorf("guest: build the oidc_login block: %w", err)
	}
	changed := true
	if cur := mapGet(server, "oidc_login"); cur != nil {
		same, err := sameYAML(cur, &want)
		if err != nil {
			return nil, false, err
		}
		changed = !same
	}
	if changed {
		mapSet(server, "oidc_login", &want)
	}
	// Both writes feed one "changed": either alone must restart the hub, since
	// scion reads the whole file once at startup.
	if setOperatorDisplayName(server) {
		changed = true
	}
	if enableMessageBroker(server) {
		changed = true
	}
	if !changed {
		return existing, false, nil
	}

	out, err := encodeSettings(&doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// setOperatorDisplayName names an operator in `server.auth` when nothing there
// names one already, and reports whether it wrote. See operatorDisplayName for
// why this is needed and why it is inert.
//
// It only ever ADDS: an existing display_name, email or username is an
// operator's own identity and is left exactly as it is, so this cannot rename
// a user on a re-apply. `auth` holds unrelated keys too (user_access_mode), so
// the block is merged into, never replaced.
func setOperatorDisplayName(server *yaml.Node) bool {
	auth := mapGet(server, "auth")
	if auth != nil {
		if auth.Kind != yaml.MappingNode {
			// Not a shape lever can reason about — leave it for the operator.
			return false
		}
		for _, k := range []string{"display_name", "email", "username"} {
			if n := mapGet(auth, k); n != nil && n.Value != "" {
				return false
			}
		}
	} else {
		auth = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapSet(server, "auth", auth)
	}
	mapSet(auth, "display_name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: operatorDisplayName})
	return true
}

// enableMessageBroker fills in `server.message_broker.enabled` when the key is
// absent, and reports whether it changed anything.
//
// Scion registers the /api/v1/chat/* ROUTES by default but wires the store
// that backs them inside `if MessageBroker != nil && MessageBroker.Enabled`
// (cmd/server_foreground.go). With the key absent — scion's own zero value —
// the web channel spoke never registers, so native chat answers 503 "Chat
// preferences not available" on a hub whose chat UI is present and inviting.
// The feature looks shipped and is inert.
//
// Absent-only, never an override: an operator who has written `enabled: false`
// has made a choice, and re-applying must not silently undo it. Same reason
// setOperatorDisplayName leaves a name the operator already set.
func enableMessageBroker(server *yaml.Node) bool {
	mb := mapGet(server, "message_broker")
	if mb != nil {
		if mb.Kind != yaml.MappingNode {
			// Not a shape lever can reason about — leave it for the operator.
			return false
		}
		if n := mapGet(mb, "enabled"); n != nil && n.Value != "" {
			return false
		}
	} else {
		mb = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapSet(server, "message_broker", mb)
	}
	mapSet(mb, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	return true
}

// clearOperatorDisplayName undoes setOperatorDisplayName and only that: it
// removes `server.auth.display_name` when it still holds the exact value lever
// wrote, then drops an `auth` mapping that is left empty. Any other value, or
// any other key beside it, is an operator's own and stays.
func clearOperatorDisplayName(server *yaml.Node) bool {
	auth := mapGet(server, "auth")
	if auth == nil || auth.Kind != yaml.MappingNode {
		return false
	}
	if n := mapGet(auth, "display_name"); n == nil || n.Value != operatorDisplayName {
		return false
	}
	mapDelete(auth, "display_name")
	if len(auth.Content) == 0 {
		mapDelete(server, "auth")
	}
	return true
}

// hubSettingsWithoutLogin returns the settings content for a hub with remote
// login OFF, and whether anything changed. Of the three edits
// hubSettingsConverged makes it reverts two — the `oidc_login` block, and the
// `auth.display_name` lever wrote (clearOperatorDisplayName) — because both
// exist only to serve the login path.
//
// `message_broker.enabled` is deliberately left as it is. It enables native
// chat, a hub feature that does not depend on remote login; turning remote
// access off must not also silently break chat for the operator on the host.
// The key is absent-only on the way in (an operator's `false` is never
// overridden) and, for the same reason, untouched on the way out: the
// documented opt-out is to set it to false in the jail's settings.
//
// Every "nothing to do" shape — an empty file, a file that is not a mapping,
// no `server:` key, no block under it — is (unchanged, false, nil): removal
// is convergence, not an assertion about what was there.
func hubSettingsWithoutLogin(existing []byte) ([]byte, bool, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return existing, false, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, false, fmt.Errorf("guest: parse the jail's %s: %w", scionSettingsRel, err)
	}
	root := documentRoot(&doc)
	if root == nil {
		return existing, false, nil
	}
	server := mapGet(root, "server")
	if server == nil || server.Kind != yaml.MappingNode {
		return existing, false, nil
	}
	changed := false
	if mapGet(server, "oidc_login") != nil {
		mapDelete(server, "oidc_login")
		changed = true
	}
	if clearOperatorDisplayName(server) {
		changed = true
	}
	if !changed {
		return existing, false, nil
	}

	out, err := encodeSettings(&doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// encodeSettings renders an edited settings document back to bytes.
func encodeSettings(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// Two-space indent: scion's own settings files are written that way, and
	// this file is read by people as often as by programs.
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("guest: render %s: %w", scionSettingsRel, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("guest: render %s: %w", scionSettingsRel, err)
	}
	return buf.Bytes(), nil
}

// documentRoot returns the mapping at the top of a parsed document, creating
// the document structure when the file was empty. nil when the file holds
// something that is not a mapping (a list, a scalar), which callers refuse
// rather than overwrite.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == 0 {
		root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{root}
		return root
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return doc.Content[0]
}

// mapGet returns the value node for key in a mapping, or nil.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapSet replaces key's value in a mapping, or appends the pair when absent.
// Appending keeps the file's existing order intact, so a diff of a changed
// settings file shows only what lever added.
func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// mapDelete removes a key/value pair from a mapping, leaving the rest of the
// file's order intact.
func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// sameYAML reports whether two nodes serialise identically — the semantic
// comparison the change detection rests on.
func sameYAML(a, b *yaml.Node) (bool, error) {
	ab, err := yaml.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("guest: compare the oidc_login block: %w", err)
	}
	bb, err := yaml.Marshal(b)
	if err != nil {
		return false, fmt.Errorf("guest: compare the oidc_login block: %w", err)
	}
	return bytes.Equal(ab, bb), nil
}
