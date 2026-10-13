package guest

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

const (
	loginDisplayName = "Lever remote access"

	// operatorDisplayName names an operator in the hub's `server.auth` block
	// only to clear scion's first-run wizard: the SPA redirects every load to
	// /onboarding until `server.auth` carries a display_name, email or
	// username.
	//
	// It is inert otherwise: scion reads those fields only when dev auth is
	// on, and lever runs --dev-auth=false. It is deliberately NOT an
	// operator's email, so it cannot compete with the per-operator hub users
	// keyed on their tailnet login.
	operatorDisplayName = "Lever"
)

// removeHubLoginSettings is fail-soft on a file it cannot read or parse: the
// forwarder is already gone, so what is left only names a dead provider, and
// "no change" is the honest answer. A failed WRITE stays fatal: reporting no
// change would claim the guest is converged when it is not.
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

func (g Guest) ensureHubLoginSettings(ctx context.Context, spec types.HubLogin) (bool, error) {
	res, err := g.UserRun(ctx, "/bin/bash", "-c", readScionSettingsScript)
	if err != nil {
		// Fatal rather than "assume empty", which would rewrite scion's
		// machine configuration from nothing.
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

// writeScionSettings writes as the RUN USER: it is that user's own config,
// not root's.
func (g Guest) writeScionSettings(ctx context.Context, content []byte) error {
	if err := g.pipeInto(ctx, g.UserPrefix, bytes.NewReader(content), writeScionSettingsScript); err != nil {
		return fmt.Errorf("guest: write %s: %w", layout.SettingsRel, err)
	}
	return nil
}

// readScionSettingsScript reports an absent settings.yaml as empty content
// rather than as a failure.
var readScionSettingsScript = fmt.Sprintf(`set -u
if [ -f "$HOME/%s" ]; then echo "LEGACY 1"; else echo "LEGACY 0"; fi
if [ -f "$HOME/%s" ]; then cat "$HOME/%s"; fi
`, layout.ServerYAMLRel, layout.SettingsRel, layout.SettingsRel)

func parseScionSettingsRead(out string) (settings []byte, hasServerYAML bool, err error) {
	line, rest, ok := strings.Cut(out, "\n")
	if !ok || !strings.HasPrefix(line, "LEGACY ") {
		return nil, false, fmt.Errorf("guest: could not read the jail's scion settings (unexpected output)")
	}
	return []byte(rest), strings.TrimSpace(strings.TrimPrefix(line, "LEGACY ")) == "1", nil
}

var writeScionSettingsScript = fmt.Sprintf(`mkdir -p "$HOME/%s" && cat > "$HOME/%s.lever-tmp" && mv "$HOME/%s.lever-tmp" "$HOME/%s"`,
	filepath.Dir(layout.SettingsRel), layout.SettingsRel, layout.SettingsRel, layout.SettingsRel)

// hubSettingsConverged converges `oidc_login`, `auth.display_name` and
// `message_broker.enabled` under `server:`; each is restart-worthy on its own.
//
// It edits the parsed document, so every other key and comment survives.
// "Changed" is decided semantically: a re-serialisation that only moves
// whitespace must not read as a change, or every apply would restart the hub.
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
		// settings.yaml wins outright once it has a `server` key, so adding
		// one would make scion silently stop reading server.yaml.
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

// setOperatorDisplayName only ever ADDS: an existing display_name, email or
// username is the operator's own and is left as it is. `auth` holds unrelated
// keys too, so it is merged into, never replaced.
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

// enableMessageBroker sets `server.message_broker.enabled` only when absent:
// without it scion's native chat answers 503 "Chat preferences not
// available". An operator's explicit `enabled: false` is never overridden.
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

// clearOperatorDisplayName undoes setOperatorDisplayName and only that: any
// other value, or any other key beside it, is the operator's own and stays.
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

// hubSettingsWithoutLogin reverts `oidc_login` and lever's
// `auth.display_name`, which serve only the login path.
// `message_broker.enabled` is deliberately left: chat does not depend on
// remote login.
//
// Every "nothing to do" shape is (unchanged, false, nil): removal is
// convergence, not an assertion about what was there.
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

func encodeSettings(doc *yaml.Node) ([]byte, error) {
	out, err := layout.EncodeSettings(doc)
	if err != nil {
		return nil, fmt.Errorf("guest: render %s: %w", layout.SettingsRel, err)
	}
	return out, nil
}
