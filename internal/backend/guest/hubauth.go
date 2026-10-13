package guest

import (
	"bytes"
	"context"
	"fmt"
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
	updated, oidcRemoved, changed, err := hubAuthSettingsConverged(existing, spec, hasServerYAML)
	if err != nil {
		return false, err
	}
	if changed {
		if err := g.writeScionSettings(ctx, updated); err != nil {
			return false, err
		}
	}
	return keyChanged || oidcRemoved, nil
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
// it), writes lever's auth keys and admin_emails, and keeps the web-only
// settings in line with RemoteOn. It edits the parsed document, so every other
// key and comment survives, and decides "changed" semantically. Keys under
// `auth` lever does not own (email, display_name: scion's identity route
// writes them) are left alone.
func hubAuthSettingsConverged(existing []byte, spec types.HubAuth, hasServerYAML bool) (out []byte, oidcRemoved, changed bool, err error) {
	doc, err := layout.ParseSettings(existing)
	if err != nil {
		return nil, false, false, fmt.Errorf("guest: parse the jail's %s: %w", layout.SettingsRel, err)
	}
	root := layout.DocumentRoot(doc)
	if root == nil {
		return nil, false, false, fmt.Errorf("guest: the jail's %s is not a YAML mapping", layout.SettingsRel)
	}
	server := layout.MapGet(root, layout.KeyServer)
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
		oidcRemoved, changed = true, true
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
	hub := layout.MapGet(server, "hub")
	if hub != nil && hub.Kind != yaml.MappingNode {
		return nil, false, false, fmt.Errorf("guest: `server.hub` in the jail's %s is not a mapping", layout.SettingsRel)
	}
	if hub == nil {
		hub = layout.NewMapping()
		layout.MapSet(server, "hub", hub)
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
	return out, oidcRemoved, true, nil
}
