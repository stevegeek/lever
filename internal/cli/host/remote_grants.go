package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// remoteGrants is what each login may do under app (see state.RemoteGrants).
func remoteGrants(app *config.App) state.RemoteGrants {
	g := state.RemoteGrants{Enabled: app.RemoteEnabled(), Logins: map[string]string{}}
	if !g.Enabled {
		return g
	}
	if len(app.Remote.AllowedUsers) == 0 {
		g.Logins[""] = grantDigest(app, config.RemoteUser{})
		return g
	}
	for _, u := range app.Remote.AllowedUsers {
		g.Logins[u.Login] = grantDigest(app, u)
	}
	return g
}

// grantDigest covers everything the proxy decides per login from its start-up
// config. A change in any of it, wider or narrower, refuses the login until
// the apply's proxy restart: a stale proxy must never keep more than the
// config now grants, and telling wider from narrower is not worth the risk.
func grantDigest(app *config.App, u config.RemoteUser) string {
	agents, see := slices.Clone(u.Agents), slices.Clone(u.See)
	slices.Sort(agents)
	slices.Sort(see)
	b, _ := json.Marshal(struct {
		Tier      string   `json:"tier"`
		Agents    []string `json:"agents"`
		See       []string `json:"see"`
		Chat      bool     `json:"chat"`
		Files     bool     `json:"files"`
		Uploads   bool     `json:"uploads"`
		Shares    bool     `json:"shares"`
		Voice     bool     `json:"voice"`
		ReadAloud string   `json:"read_aloud"`
	}{
		Tier: u.EffectiveTier(), Agents: agents, See: see,
		Chat:  app.RemoteLandingChat(),
		Files: app.FilesOn() && u.FilesAllowed(), Uploads: app.FilesUploadsOn(), Shares: app.FilesSharesOn(),
		Voice: app.VoiceOn() && u.VoiceAllowed(), ReadAloud: string(app.EffectiveReadAloud()),
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fenceRemoteLogins is Deps.FenceRemoteLogins: it writes the grants before
// anything else in the apply, and stops the proxy when remote access is off.
func fenceRemoteLogins(app *config.App, st state.State, stop func() error) error {
	if err := st.SaveRemoteGrants(remoteGrants(app)); err != nil {
		return fmt.Errorf("writing the remote login grants: %w", err)
	}
	if !app.RemoteEnabled() {
		return stop()
	}
	return nil
}

var errRemoteGrantChanged = errors.New("this login's remote access changed since the proxy started")

// remoteLoginCurrent is the proxy's remoteproxy.Config.LoginCurrent: the
// login's grant at start-up must still be the one apply last wrote. The file
// is read per call, as remoteContactUser reads remote-role.json, and any
// failure to read it refuses.
func remoteLoginCurrent(app *config.App, st state.State) func(login string) error {
	mine := remoteGrants(app).Logins
	return func(login string) error {
		g, err := st.LoadRemoteGrants()
		if err != nil {
			return err
		}
		want, ok := mine[login]
		if got, applied := g.Logins[login]; !g.Enabled || !ok || !applied || got != want {
			return errRemoteGrantChanged
		}
		return nil
	}
}
