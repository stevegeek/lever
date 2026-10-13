package guest

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

const (
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

func encodeSettings(doc *yaml.Node) ([]byte, error) {
	out, err := layout.EncodeSettings(doc)
	if err != nil {
		return nil, fmt.Errorf("guest: render %s: %w", layout.SettingsRel, err)
	}
	return out, nil
}
