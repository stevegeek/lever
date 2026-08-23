package guest

import (
	"bytes"
	"context"
	"fmt"
	"gopkg.in/yaml.v3"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// Convergence of the hub's ~/.scion/settings.yaml for the remote-access login
// path (the `oidc_login` block and its neighbours). See hublogin.go for the
// orchestration that calls into here.

const (
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

// ensureHubLoginSettings writes the oidc_login block into the guest's
// ~/.scion/settings.yaml, and reports whether the file changed.
func (g Guest) ensureHubLoginSettings(ctx context.Context, spec types.HubLogin) (bool, error) {
	res, err := g.UserRun(ctx, "/bin/bash", "-c", readScionSettingsScript)
	if err != nil {
		// Deliberately fatal rather than "assume empty": treating an
		// unreadable settings file as absent would rewrite scion's machine
		// configuration from nothing.
		return false, fmt.Errorf("guest: read %s: %w", layout.SettingsRel, err)
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
		return fmt.Errorf("guest: write %s: %w", layout.SettingsRel, err)
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
`, layout.ServerYAMLRel, layout.SettingsRel, layout.SettingsRel)

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
	filepath.Dir(layout.SettingsRel), layout.SettingsRel, layout.SettingsRel, layout.SettingsRel)

// hubSettingsConverged returns the settings file content lever wants for a hub
// with remote login ON, and whether it differs from what is already there. It
// converges THREE things under `server:`, each restart-worthy on its own
// because scion reads the whole file once at startup:
//
//   - `oidc_login`: lever's login block (layout.OIDCLogin), replaced whenever it
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
func hubSettingsConverged(existing []byte, spec types.HubLogin, hasServerYAML bool) ([]byte, bool, error) {
	doc, err := layout.ParseSettings(existing)
	if err != nil {
		return nil, false, fmt.Errorf("guest: parse the jail's %s: %w", layout.SettingsRel, err)
	}
	root := layout.DocumentRoot(doc)
	if root == nil {
		return nil, false, fmt.Errorf("guest: the jail's %s is not a YAML mapping", layout.SettingsRel)
	}
	server := layout.MapGet(root, layout.KeyServer)
	if server == nil && hasServerYAML {
		// Adding a `server` key here would silently move scion's whole server
		// configuration from the legacy server.yaml to this file, because
		// settings.yaml wins outright once it has that key (pkg/config/
		// hub_config.go loadGlobalConfigFromSettings). Whatever server.yaml
		// holds would stop being read. Refuse rather than guess.
		return nil, false, fmt.Errorf("guest: the jail has a legacy ~/%s and no `server:` key in ~/%s — "+
			"adding one would make scion ignore server.yaml entirely. Consolidate them first "+
			"(`scion config migrate --server` in the jail), then re-run `lever apply`",
			layout.ServerYAMLRel, layout.SettingsRel)
	}
	if server == nil {
		server = layout.NewMapping()
		layout.MapSet(root, layout.KeyServer, server)
	}
	if server.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("guest: `server:` in the jail's %s is not a mapping", layout.SettingsRel)
	}

	want, err := layout.OIDCLogin{
		Enabled:     true,
		DisplayName: loginDisplayName,
		IssuerURL:   fmt.Sprintf("http://127.0.0.1:%d", spec.IssuerPort),
		ClientID:    spec.ClientID,
	}.Node()
	if err != nil {
		return nil, false, fmt.Errorf("guest: build the oidc_login block: %w", err)
	}
	changed := true
	if cur := layout.MapGet(server, layout.KeyOIDCLogin); cur != nil {
		same, err := layout.SameYAML(cur, want)
		if err != nil {
			return nil, false, fmt.Errorf("guest: compare the oidc_login block: %w", err)
		}
		changed = !same
	}
	if changed {
		layout.MapSet(server, layout.KeyOIDCLogin, want)
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

	out, err := encodeSettings(doc)
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
	auth := layout.MapGet(server, layout.KeyAuth)
	if auth != nil {
		if auth.Kind != yaml.MappingNode {
			// Not a shape lever can reason about — leave it for the operator.
			return false
		}
		for _, k := range []string{layout.KeyDisplayName, layout.KeyEmail, layout.KeyUsername} {
			if n := layout.MapGet(auth, k); n != nil && n.Value != "" {
				return false
			}
		}
	} else {
		auth = layout.NewMapping()
		layout.MapSet(server, layout.KeyAuth, auth)
	}
	layout.MapSet(auth, layout.KeyDisplayName, layout.StringNode(operatorDisplayName))
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
	mb := layout.MapGet(server, layout.KeyMessageBroker)
	if mb != nil {
		if mb.Kind != yaml.MappingNode {
			// Not a shape lever can reason about — leave it for the operator.
			return false
		}
		if n := layout.MapGet(mb, layout.KeyEnabled); n != nil && n.Value != "" {
			return false
		}
	} else {
		mb = layout.NewMapping()
		layout.MapSet(server, layout.KeyMessageBroker, mb)
	}
	layout.MapSet(mb, layout.KeyEnabled, layout.BoolNode(true))
	return true
}

// clearOperatorDisplayName undoes setOperatorDisplayName and only that: it
// removes `server.auth.display_name` when it still holds the exact value lever
// wrote, then drops an `auth` mapping that is left empty. Any other value, or
// any other key beside it, is an operator's own and stays.
func clearOperatorDisplayName(server *yaml.Node) bool {
	auth := layout.MapGet(server, layout.KeyAuth)
	if auth == nil || auth.Kind != yaml.MappingNode {
		return false
	}
	if n := layout.MapGet(auth, layout.KeyDisplayName); n == nil || n.Value != operatorDisplayName {
		return false
	}
	layout.MapDelete(auth, layout.KeyDisplayName)
	if len(auth.Content) == 0 {
		layout.MapDelete(server, layout.KeyAuth)
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
	doc, err := layout.ParseSettings(existing)
	if err != nil {
		return nil, false, fmt.Errorf("guest: parse the jail's %s: %w", layout.SettingsRel, err)
	}
	root := layout.DocumentRoot(doc)
	if root == nil {
		return existing, false, nil
	}
	server := layout.MapGet(root, layout.KeyServer)
	if server == nil || server.Kind != yaml.MappingNode {
		return existing, false, nil
	}
	changed := false
	if layout.MapGet(server, layout.KeyOIDCLogin) != nil {
		layout.MapDelete(server, layout.KeyOIDCLogin)
		changed = true
	}
	if clearOperatorDisplayName(server) {
		changed = true
	}
	if !changed {
		return existing, false, nil
	}

	out, err := encodeSettings(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// encodeSettings renders an edited settings document back to bytes, naming
// the file in the error.
func encodeSettings(doc *yaml.Node) ([]byte, error) {
	out, err := layout.EncodeSettings(doc)
	if err != nil {
		return nil, fmt.Errorf("guest: render %s: %w", layout.SettingsRel, err)
	}
	return out, nil
}
