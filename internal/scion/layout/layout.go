// Package layout is lever's record of where scion keeps things on disk and how
// its machine-level settings file is shaped. It is pure data and pure
// functions — no exec, no guest transport — so every package that has to name
// a scion path or edit scion's settings.yaml (internal/backend/guest,
// internal/provision/webassets) can import it without dragging the scion CLI
// client (internal/scion) or a runner along.
//
// Everything here mirrors a fact about scion's own code, and each constant
// names the upstream file it was read from. When scion moves a file or renames
// a key, this is the one place to change.
//
// What does NOT live here: anything that names a scion subcommand, flag or
// output wording. That is internal/scion (see its package doc).
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
	// (pkg/config/settings_v1.go). It is ALSO the per-project settings file,
	// relative to a project-configs entry (see ProjectConfigsRel).
	SettingsRel = Dir + "/settings.yaml"

	// ServerYAMLRel is scion's legacy server configuration file. Once
	// settings.yaml carries a `server:` key scion reads that and ignores this
	// file outright (pkg/config/hub_config.go loadGlobalConfigFromSettings).
	ServerYAMLRel = Dir + "/server.yaml"

	// ProjectConfigsRel holds one directory per registered project, each with
	// its own SettingsRel beneath it (e.g. lever__c857bb16/.scion/settings.yaml).
	ProjectConfigsRel = Dir + "/project-configs"

	// TemplatesRel is the global agent-template directory. Scion force-rewrites
	// only the `default` entry on every server start
	// (config.UpdateDefaultTemplates); any other name is left alone.
	TemplatesRel = Dir + "/templates"
)

// ProjectMarker is the in-tree marker scion writes under a registered
// project's workspace (<workspace>/.scion) to record that the project is
// initialized.
const ProjectMarker = ".scion"

// WorkspacePathKey is the top-level key in a project-configs settings.yaml
// that names the workspace the registration belongs to.
const WorkspacePathKey = "workspace_path"

// WebAssetsSentinel is the file scion itself treats as proof that a built web
// asset set is usable: cmd/server_foreground.go warns "assets are incomplete
// (main.js missing)" when it cannot stat this path, and pkg/hub/web.go's
// Go-rendered app shell loads exactly this URL. Slash-separated: it is both a
// URL path and a path under the dist root.
const WebAssetsSentinel = "assets/main.js"

// Keys in the machine-level settings.yaml, as scion's V1 settings schema names
// them (pkg/config/settings_v1.go).
const (
	// KeyServer is the top-level block holding the hub's server configuration.
	KeyServer = "server"
	// KeyOIDCLogin is the OIDC login block under KeyServer (V1OIDCLoginConfig).
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
)

// OIDCLogin is scion's oidc_login block as it appears under `server:` in
// settings.yaml (pkg/config/settings_v1.go V1OIDCLoginConfig).
//
// Four keys, and deliberately not the other two. client_secret is omitted
// because lever's provider is a public client and scion only sends the
// parameter when it is non-empty. scopes is omitted because scion's default is
// already exactly "openid email profile" (pkg/hub/oauth.go oidcScopes), and a
// config file should not restate a default.
type OIDCLogin struct {
	Enabled     bool   `yaml:"enabled"`
	DisplayName string `yaml:"display_name"`
	IssuerURL   string `yaml:"issuer_url"`
	ClientID    string `yaml:"client_id"`
}

// Node encodes the block as a YAML mapping node, ready for MapSet.
func (o OIDCLogin) Node() (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(o); err != nil {
		return nil, fmt.Errorf("encode the %s block: %w", KeyOIDCLogin, err)
	}
	return &n, nil
}

// ParseSettings parses a settings file into a document node. Empty (or
// whitespace-only) content parses to a zero node, which DocumentRoot turns
// into an empty mapping — an absent file is a file with nothing in it.
func ParseSettings(content []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(content)) > 0 {
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return nil, err
		}
	}
	return &doc, nil
}

// EncodeSettings renders an edited settings document back to bytes.
//
// Two-space indent: scion's own settings files are written that way, and the
// file is read by people as often as by programs.
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

// MapSet replaces key's value in a mapping, or appends the pair when absent.
// Appending keeps the file's existing order intact, so a diff of a changed
// settings file shows only what was added.
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

// SameYAML reports whether two nodes serialise identically — the semantic
// comparison change detection rests on, so a re-serialisation that only moves
// whitespace never reads as a change.
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
