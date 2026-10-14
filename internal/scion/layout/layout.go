// Package layout is lever's record of where scion keeps things on disk and how
// its machine-level settings file is shaped. It must stay pure data and pure
// functions (no exec, no guest transport) so any package can import it.
//
// Each constant mirrors a fact about scion's code and names the upstream file
// it was read from. Subcommands, flags and output wording belong in
// internal/scion instead.
package layout

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Paths relative to the scion user's home directory (the jail run user's).
const (
	// Dir is scion's per-user state directory.
	Dir = ".scion"

	// SettingsRel is scion's machine-level settings file
	// (pkg/config/settings_v1.go). It is also the per-project settings file,
	// relative to a project-configs entry.
	SettingsRel = Dir + "/settings.yaml"

	// ServerYAMLRel is scion's legacy server configuration file, ignored once
	// settings.yaml carries a server: key (pkg/config/hub_config.go).
	ServerYAMLRel = Dir + "/server.yaml"

	// ProjectConfigsRel holds one directory per registered project, each with
	// its own SettingsRel beneath it (e.g. lever__c857bb16/.scion/settings.yaml).
	ProjectConfigsRel = Dir + "/project-configs"

	// TemplatesRel is the global agent-template directory. scion rewrites only
	// the default entry on every server start (config.UpdateDefaultTemplates).
	TemplatesRel = Dir + "/templates"

	// DevTokenRel is the residual dev-auth admin token scion writes on first
	// start (<scionDir>/dev-token). lever deletes it once apply is done.
	DevTokenRel = Dir + "/dev-token"

	// ServerPIDRel is the pid file of scion's server daemon
	// (pkg/daemon/daemon.go). scion keeps one per home, whatever port the
	// daemon binds, and scion server stop signals whatever pid it names after
	// only a signal-0 liveness check.
	ServerPIDRel = Dir + "/server.pid"
	// ServerLogRel is the server daemon's log (pkg/daemon/daemon.go
	// LogFileName("server")), where a start that failed says why.
	ServerLogRel = Dir + "/server.log"
)

// ProjectMarker is the in-tree marker scion writes under a registered
// project's workspace (<workspace>/.scion) to record that the project is
// initialized.
const ProjectMarker = ".scion"

// WorkspacePathKey is the top-level key in a project-configs settings.yaml
// that names the workspace the registration belongs to.
const WorkspacePathKey = "workspace_path"

// WebAssetsSentinel is the file scion treats as proof that a built web asset
// set is usable (cmd/server_foreground.go, pkg/hub/web.go). Slash-separated:
// it is both a URL path and a path under the dist root.
const WebAssetsSentinel = "assets/main.js"

// WebAssetsDir is where lever stages scion's built SPA inside the guest, and
// the value it passes to scion server start --web-assets-dir. It is lever's
// choice, not a scion fact.
const WebAssetsDir = "/usr/local/share/scion/web"

// Keys in the machine-level settings.yaml, as scion's V1 settings schema names
// them (pkg/config/settings_v1.go).
const (
	// KeyServer is the top-level block holding the hub's server configuration.
	KeyServer = "server"
	// KeyOIDCLogin is the OIDC login block under KeyServer (V1OIDCLoginConfig);
	// lever removes it, since scion offers OAuth whenever it is present.
	KeyOIDCLogin = "oidc_login"
	// KeyAuth is the dev-user/identity block under KeyServer (hub.DevUserConfig).
	KeyAuth = "auth"
	// KeyDisplayName, KeyEmail and KeyUsername are the identity fields under
	// KeyAuth; any one of them non-empty makes the hub report IdentitySet.
	KeyDisplayName = "display_name"
	KeyEmail       = "email"
	KeyUsername    = "username"
	// KeyMessageBroker is the native-chat block under KeyServer.
	KeyMessageBroker = "message_broker"
	// KeyEnabled is the boolean that switches a block on.
	KeyEnabled = "enabled"
	// KeyTelemetry is the top-level telemetry block, a sibling of KeyServer.
	// scion turns it into every agent's SCION_TELEMETRY_* env at start
	// (pkg/config/telemetry_convert.go).
	KeyTelemetry = "telemetry"
)

// ParseSettings parses a settings file into a document node. Empty content
// parses to a zero node, which DocumentRoot turns into an empty mapping.
func ParseSettings(content []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(content)) > 0 {
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return nil, err
		}
	}
	return &doc, nil
}

// EncodeSettings renders an edited settings document back to bytes with
// scion's two-space indent.
func EncodeSettings(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DocumentRoot returns the mapping at the top of a parsed document, creating
// the document structure when the file was empty. nil when the file holds
// something that is not a mapping (a list, a scalar), which callers refuse
// rather than overwrite.
func DocumentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == 0 {
		root := NewMapping()
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

// NewMapping returns an empty mapping node.
func NewMapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

// StringNode returns a scalar string node.
func StringNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// BoolNode returns a scalar boolean node.
func BoolNode(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(v)}
}

// MapGet returns the value node for key in a mapping, or nil.
func MapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// MapSet replaces key's value in a mapping, or appends the pair when absent,
// keeping the file's existing order.
func MapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, StringNode(key), val)
}

// MapDelete removes a key/value pair from a mapping, leaving the rest of the
// file's order intact.
func MapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// SameYAML reports whether two nodes serialise identically, so a
// re-serialisation that only moves whitespace never reads as a change.
func SameYAML(a, b *yaml.Node) (bool, error) {
	ab, err := yaml.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := yaml.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}
