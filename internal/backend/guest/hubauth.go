package guest

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

// HubAuthKeyPath holds the public half of lever's assertion key. Root-owned
// and outside every home: scion's workstation routes let any authenticated
// loopback caller (an agent included) write files under $HOME, and the hub
// must not read a key an agent could have replaced.
const (
	HubAuthKeyPath = hubauth.GuestPublicKeyPath
	HubAuthKeyDir  = "/etc/lever"
)

// LoginForwardPath is a guest login forwarder that bridges guest loopback to
// a host port. Nothing uses it with proxy auth, and a bridge left in place
// reaches whatever later binds that host port, so EnsureHubAuth removes it.
const LoginForwardPath = "/usr/local/bin/lever-login-forward"

// removeLoginForwardScript runs as root, which can signal the run user's
// process and whose PATH no run user can write. pkill exit 1 means nothing
// matched; above that the kill did not happen, and removing the binary while
// its process keeps bridging is the state this exists to prevent.
const removeLoginForwardScript = `set -u
if [ ! -e ` + LoginForwardPath + ` ]; then exit 0; fi
pkill -f '^` + LoginForwardPath + `'
rc=$?
if [ $rc -gt 1 ]; then echo "could not stop the login forwarder (pkill exit $rc)" >&2; exit 1; fi
rm -f ` + LoginForwardPath + `
`

// installHubAuthKeyScript writes stdin as the key file through a temp file and
// a rename, so the hub never reads a half-written key. A symlink or non-root
// owner at the directory is refused rather than repaired: something other
// than lever put it there. It prints CHANGED or SAME.
const installHubAuthKeyScript = `set -eu
d=` + HubAuthKeyDir + `
f=` + HubAuthKeyPath + `
if [ -L "$d" ]; then echo "$d is a symlink; refusing to install the hub key" >&2; exit 1; fi
if [ -e "$d" ] && [ "$(stat -c %u "$d")" != 0 ]; then echo "$d is not owned by root; refusing to install the hub key" >&2; exit 1; fi
install -d -o root -g root -m 0755 "$d"
tmp=$(mktemp "$d/.hub-auth.pub.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
if [ -f "$f" ] && [ ! -L "$f" ] && cmp -s "$tmp" "$f" && [ "$(stat -c '%u %a' "$f")" = "0 644" ]; then echo SAME; exit 0; fi
chown root:root "$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$f"
trap - EXIT
echo CHANGED
`

// removeDevTokenScript removes scion's dev-auth admin token, which a hub
// that once ran with dev auth left behind; a hub with dev auth off never
// reads it, but it stays an admin credential for any later dev-auth start.
var removeDevTokenScript = `rm -f -- "$HOME/` + layout.DevTokenRel + `"`

// EnsureHubAuth: see backend.Backend.EnsureHubAuth.
func (g Guest) EnsureHubAuth(ctx context.Context, spec types.HubAuth) (bool, error) {
	if len(bytes.TrimSpace(spec.PublicKeyPEM)) == 0 || spec.Issuer == "" || spec.Audience == "" || spec.AdminEmail == "" || spec.Header == "" {
		return false, fmt.Errorf("guest: hub auth: public key, issuer, audience, admin email and header are all required")
	}
	if _, err := g.RootRun(ctx, "bash", "-c", removeLoginForwardScript); err != nil {
		return false, fmt.Errorf("guest: remove the old login forwarder: %w", err)
	}
	argv := append(append([]string{}, g.RootPrefix[1:]...), "bash", "-c", installHubAuthKeyScript)
	res, err := g.Host.RunStdin(ctx, bytes.NewReader(spec.PublicKeyPEM), nil, g.RootPrefix[0], argv...)
	if err != nil {
		return false, fmt.Errorf("guest: install %s: %w: %s", HubAuthKeyPath, err, strings.TrimSpace(res.Stderr))
	}
	keyChanged := strings.TrimSpace(res.Stdout) == "CHANGED"
	if _, err := g.UserRun(ctx, "/bin/bash", "-c", removeDevTokenScript); err != nil {
		return false, fmt.Errorf("guest: remove scion's dev token ~/%s: %w", layout.DevTokenRel, err)
	}

	read, err := g.UserRun(ctx, "/bin/bash", "-c", readScionSettingsScript)
	if err != nil {
		// Fatal rather than "assume empty", which would rewrite scion's machine
		// configuration from nothing.
		return false, fmt.Errorf("guest: read %s: %w", layout.SettingsRel, err)
	}
	existing, hasServerYAML, err := parseScionSettingsRead(read.Stdout)
	if err != nil {
		return false, err
	}
	updated, restart, changed, err := hubAuthSettingsConverged(existing, spec, hasServerYAML)
	if err != nil {
		return false, err
	}
	if changed {
		if err := g.writeScionSettings(ctx, updated); err != nil {
			return false, err
		}
	}
	return keyChanged || restart, nil
}

// keyConduit is the hub's relay block (server.hub.conduit), which lever never
// writes.
const keyConduit = "conduit"

// plainKeys refuses a mapping on the path to server.hub.conduit with a key
// lever cannot read as the hub will: an alias or a complex key (a node's
// Value is then not the key's text) or a tag that decodes to other text
// (!!binary). path names the mapping in the error.
func plainKeys(m *yaml.Node, path string) error {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		switch {
		case k.Kind != yaml.ScalarNode: // refused below
		case k.Tag == "" || k.Tag == "!" || strings.HasPrefix(k.Tag, "!!") && k.Tag != "!!binary":
			continue
		}
		return fmt.Errorf("guest: `%s` in the jail's %s has a key that is an alias, a complex key or a tagged value (%s); lever cannot tell which key the hub reads from it. Write it as a plain key and re-run `lever apply`",
			path, layout.SettingsRel, strconv.Quote(k.Tag+" "+k.Value))
	}
	return nil
}

// mapGetExact is layout.MapGet for a key on the path to server.hub.conduit.
// The hub reads settings.yaml with exact-case keys, but other scion readers
// (koanf with mapstructure) match keys in any case, so a key that differs from
// key only in case, a repeated key, and a key plainKeys refuses are all
// refused: which of them a reader takes is not lever's to guess.
func mapGetExact(m *yaml.Node, key, path string) (*yaml.Node, error) {
	if err := plainKeys(m, path); err != nil {
		return nil, err
	}
	var found *yaml.Node
	var spellings []string
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "<<" {
			return nil, fmt.Errorf("guest: the mapping holding `%s` in the jail's %s uses a YAML merge key, which can hide keys lever must check; remove it and re-run `lever apply`",
				path, layout.SettingsRel)
		}
		if strings.EqualFold(m.Content[i].Value, key) {
			found = m.Content[i+1]
			spellings = append(spellings, strconv.Quote(m.Content[i].Value))
		}
	}
	if len(spellings) > 1 || len(spellings) == 1 && spellings[0] != strconv.Quote(key) {
		return nil, fmt.Errorf("guest: the jail's %s spells `%s` as %s, which differ from %q at most in case or repeat it; scion's readers disagree on which they take, so lever cannot check the one the hub uses. Keep one, spelled %q, and re-run `lever apply`",
			layout.SettingsRel, path, strings.Join(spellings, ", "), key, key)
	}
	return found, nil
}

// mapDeleteFold removes every pair whose key is key in any case and reports
// whether it removed one: koanf-based scion readers match keys in any case.
func mapDeleteFold(m *yaml.Node, key string) bool {
	kept := m.Content[:0]
	removed := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if strings.EqualFold(m.Content[i].Value, key) {
			removed = true
			continue
		}
		kept = append(kept, m.Content[i], m.Content[i+1])
	}
	m.Content = kept
	return removed
}

// HubAuthSettings is the auth block lever writes under `server:` for anyone
// reading settings.yaml. The hub itself takes these values from the
// SCION_SERVER_* env lever starts it with.
func HubAuthSettings(spec types.HubAuth) map[string]any {
	return map[string]any{
		"mode":             "proxy",
		"user_access_mode": "invite_only",
		"proxy": map[string]any{
			"provider": "jwt",
			"jwt": map[string]any{
				"algorithm":       "ES256",
				"header":          spec.Header,
				"issuer":          spec.Issuer,
				"audience":        spec.Audience,
				"public_key_file": HubAuthKeyPath,
			},
		},
	}
}

// hubAuthSettingsConverged drops `oidc_login` (scion builds its OAuth service
// whenever the block is present, whatever the auth mode, and env cannot unset
// it) and `hub.conduit`, reporting either as a reason to restart the hub,
// writes lever's auth keys and admin_emails, and keeps the web-only
// settings in line with RemoteOn. It edits the parsed document, so every other
// key and comment survives, and decides "changed" semantically. Keys under
// `auth` lever does not own (email, display_name: scion's identity route
// writes them) are left alone.
func hubAuthSettingsConverged(existing []byte, spec types.HubAuth, hasServerYAML bool) (out []byte, restart, changed bool, err error) {
	doc, err := layout.ParseSettings(existing)
	if err != nil {
		return nil, false, false, fmt.Errorf("guest: parse the jail's %s: %w", layout.SettingsRel, err)
	}
	root := layout.DocumentRoot(doc)
	if root == nil {
		return nil, false, false, fmt.Errorf("guest: the jail's %s is not a YAML mapping", layout.SettingsRel)
	}
	server, err := mapGetExact(root, layout.KeyServer, layout.KeyServer)
	if err != nil {
		return nil, false, false, err
	}
	if server == nil && hasServerYAML {
		// settings.yaml wins outright once it has a `server` key, so adding one
		// would make scion silently stop reading server.yaml.
		return nil, false, false, fmt.Errorf("guest: the jail has a legacy ~/%s and no `server:` key in ~/%s — "+
			"adding one would make scion ignore server.yaml entirely. Consolidate them first "+
			"(`scion config migrate --server` in the jail), then re-run `lever apply`",
			layout.ServerYAMLRel, layout.SettingsRel)
	}
	if server == nil {
		server = layout.NewMapping()
		layout.MapSet(root, layout.KeyServer, server)
	}
	if server.Kind != yaml.MappingNode {
		return nil, false, false, fmt.Errorf("guest: `server:` in the jail's %s is not a mapping", layout.SettingsRel)
	}
	if layout.MapGet(server, layout.KeyOIDCLogin) != nil {
		layout.MapDelete(server, layout.KeyOIDCLogin)
		restart, changed = true, true
	}
	auth := layout.MapGet(server, layout.KeyAuth)
	if auth != nil && auth.Kind != yaml.MappingNode {
		return nil, false, false, fmt.Errorf("guest: `server.auth` in the jail's %s is not a mapping", layout.SettingsRel)
	}
	if auth == nil {
		auth = layout.NewMapping()
		layout.MapSet(server, layout.KeyAuth, auth)
	}
	for k, v := range HubAuthSettings(spec) {
		var want yaml.Node
		if err := want.Encode(v); err != nil {
			return nil, false, false, fmt.Errorf("guest: encode server.auth.%s: %w", k, err)
		}
		if cur := layout.MapGet(auth, k); cur != nil {
			same, err := layout.SameYAML(cur, &want)
			if err != nil {
				return nil, false, false, fmt.Errorf("guest: compare server.auth.%s: %w", k, err)
			}
			if same {
				continue
			}
		}
		layout.MapSet(auth, k, &want)
		changed = true
	}
	for _, k := range []string{"dev_mode", "dev_token", "dev_token_file"} {
		if layout.MapGet(auth, k) != nil {
			layout.MapDelete(auth, k)
			changed = true
		}
	}
	hub, err := mapGetExact(server, "hub", "server.hub")
	if err != nil {
		return nil, false, false, err
	}
	if hub != nil && hub.Kind != yaml.MappingNode {
		return nil, false, false, fmt.Errorf("guest: `server.hub` in the jail's %s is not a mapping", layout.SettingsRel)
	}
	if hub == nil {
		hub = layout.NewMapping()
		layout.MapSet(server, "hub", hub)
	}
	// lever starts the hub with its conduit policy in the env, which wins over
	// the file; the rest of the block (an internal relay listener, longer
	// stream lifetimes, more allowed ports) is the relay's attack surface,
	// and agents can write this file through the workstation routes.
	if layout.MapGet(hub, "<<") != nil {
		return nil, false, false, fmt.Errorf("guest: `server.hub` in the jail's %s uses a YAML merge key, which can hide keys lever must check; remove it", layout.SettingsRel)
	}
	if err := plainKeys(hub, "server.hub"); err != nil {
		return nil, false, false, err
	}
	if mapDeleteFold(hub, keyConduit) {
		restart, changed = true, true
	}
	var admins yaml.Node
	if err := admins.Encode([]string{spec.AdminEmail}); err != nil {
		return nil, false, false, fmt.Errorf("guest: encode server.hub.admin_emails: %w", err)
	}
	if cur := layout.MapGet(hub, "admin_emails"); cur == nil {
		layout.MapSet(hub, "admin_emails", &admins)
		changed = true
	} else if same, err := layout.SameYAML(cur, &admins); err != nil {
		return nil, false, false, fmt.Errorf("guest: compare server.hub.admin_emails: %w", err)
	} else if !same {
		layout.MapSet(hub, "admin_emails", &admins)
		changed = true
	}
	if spec.RemoteOn {
		if setOperatorDisplayName(server) {
			changed = true
		}
		if enableMessageBroker(server) {
			changed = true
		}
	} else if clearOperatorDisplayName(server) {
		changed = true
	}
	if !changed {
		return existing, false, false, nil
	}
	out, err = encodeSettings(doc)
	if err != nil {
		return nil, false, false, err
	}
	return out, restart, true, nil
}
