package brokerctl

import (
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// ConfigHash digests the broker-relevant configuration. apply restarts a
// running broker whose reported hash differs, so it must cover exactly the
// config a broker bakes in at start and nothing else (a manager-image change
// must not bounce the broker). `scion.source` and `scion.binary` are paths:
// replacing the artifact behind one leaves the hash unchanged, which is why
// scion.Client.roleFlagSupported does not memoise its probe (a stale "no
// --role" would hand every agent scion's full default role).
func ConfigHash(app *config.App) string {
	// A marshal failure yields "", a guaranteed mismatch (restart).
	//
	// VerifiedChat and WebSenders derive from the remote block, which the
	// broker otherwise ignores: changing allowed_users must bounce it too.
	//
	// Optional groups (AgentMessages, Files, ManagerClaude, SharedFolders,
	// ManagerReadOnly, a worker's claude_settings and after_compact_note)
	// are omitted while unset, so an instance that does not use them keeps
	// its hash across upgrades; a new config.Worker field needs the same
	// omitempty tag or every instance with workers restarts its broker.
	//
	// ManagerClaude is in the manager's envelope: a restart reopens the
	// bootstrap latch so the next apply re-stages bootstrap.json. Shared
	// folders and read-only entries feed worker specs and guards a stale
	// broker would keep enforcing with old values.
	return state.HashJSON(struct {
		Broker          config.Broker
		Workers         []config.Worker
		Scion           config.ScionConfig
		VerifiedChat    bool
		WebSenders      []string              `json:",omitempty"`
		AgentMessages   *agentMessagesStamp   `json:",omitempty"`
		Files           *filesStamp           `json:",omitempty"`
		ManagerClaude   *wire.Claude          `json:",omitempty"`
		SharedFolders   []config.SharedFolder `json:",omitempty"`
		ManagerReadOnly []string              `json:",omitempty"`
	}{app.Broker, app.Workers, app.Scion, ChatConfigured(app), WebSenders(app), agentMessagesStampOf(app), filesStampOf(app), app.ManagerClaude(),
		app.SharedFolders, app.Manager.ReadOnly})
}

// agentMessagesStamp is the part of remote.agent_messages the broker acts on.
type agentMessagesStamp struct {
	FollowUpAfter time.Duration
	MaxChars      int
	Contacts      []string // "login=agent,agent", config order
}

func agentMessagesStampOf(app *config.App) *agentMessagesStamp {
	if !app.AgentMessagesOn() {
		return nil
	}
	s := &agentMessagesStamp{FollowUpAfter: app.EffectiveAgentFollowUpAfter(), MaxChars: app.EffectiveAgentMaxChars()}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			s.Contacts = append(s.Contacts, u.Login+"="+strings.Join(u.Agents, ","))
		}
	}
	return s
}

// filesStamp is the part of remote.files the broker acts on (share_file).
type filesStamp struct {
	MaxBytes   int64
	Extensions []string
	Contacts   []string // "login=agent,agent", config order
	Operators  []string
	// Set only when not the default, so an instance that never sets them
	// keeps its stamp.
	NoShares bool     `json:",omitempty"`
	Excluded []string `json:",omitempty"` // logins with files: false
}

func filesStampOf(app *config.App) *filesStamp {
	if !app.FilesOn() {
		return nil
	}
	s := &filesStamp{MaxBytes: app.EffectiveFilesMaxBytes(), Extensions: app.EffectiveFilesExtensions(),
		Operators: app.Remote.LoginsWithTier(config.TierOperator)}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			s.Contacts = append(s.Contacts, u.Login+"="+strings.Join(u.Agents, ","))
		}
	}
	s.NoShares, s.Excluded = !app.FilesSharesOn(), app.FilesExcludedLogins()
	return s
}

// RemoteConfigHash digests the config a `lever remote serve` process captures
// at startup (see state.RemoteConfigHash). It is the only mapping of
// config.App onto state.RemoteIdentity, so state stays free of config types.
func RemoteConfigHash(app *config.App) string {
	id := state.RemoteIdentity{
		Enabled:      app.Remote.Enabled,
		Port:         app.Remote.Port,
		BaseURL:      app.Remote.BaseURL,
		AllowedUsers: remoteUserKeys(app.Remote.AllowedUsers),
		LoginPort:    app.Remote.LoginPort,
		// Effective values, so spelling out the default (or changing the
		// header's case) does not bounce the proxy.
		IdentityHeader:     app.EffectiveRemoteIdentityHeader(),
		Bind:               app.EffectiveRemoteBind(),
		AllowWildcardBind:  app.Remote.AllowWildcardBind,
		TrustForwardedHost: app.Remote.TrustForwardedHost,
		Landing:            app.EffectiveRemoteLanding(),
		Name:               app.Name,
		Backend:            app.Backend,
		AgentMessages:      app.AgentMessagesOn(),
	}
	// Only a chat-landing proxy captures the workers, tree and labels file,
	// so a console-landing proxy does not restart on a worker change.
	if app.RemoteLandingChat() {
		id.Workers = make([]string, len(app.Workers))
		for i, w := range app.Workers {
			id.Workers[i] = w.Name
		}
		id.Tree = app.Tree
		id.LabelsFile = app.Remote.LabelsFile
		if app.FilesOn() {
			f := &state.FilesIdentity{MaxBytes: app.EffectiveFilesMaxBytes(), Extensions: app.EffectiveFilesExtensions(),
				Workspaces: []string{app.Name + "=."}}
			for _, w := range app.Workers {
				f.Workspaces = append(f.Workspaces, w.Name+"="+app.AgentWorkspaces()[w.Name])
			}
			f.NoUploads, f.NoShares, f.Excluded = !app.FilesUploadsOn(), !app.FilesSharesOn(), app.FilesExcludedLogins()
			id.Files = f
		}
		if app.VoiceOn() {
			id.Voice = &state.VoiceIdentity{Socket: app.Remote.Voice.Socket, MaxSeconds: app.EffectiveVoiceMaxSeconds(),
				Excluded: app.VoiceExcludedLogins(), NoReadAloud: app.EffectiveReadAloud() == config.ReadAloudOff}
			if app.EffectiveReadAloud() == config.ReadAloudServer {
				id.Voice.SpeakSocket = app.Remote.Voice.SpeakSocket
			}
		}
	}
	if app.PushOn() {
		id.Push = app.Remote.Push.Subject
		// Test hosts (TEST ONLY) are captured at start too; omitted when unset.
		if th := app.Remote.Push.TestHosts; len(th) > 0 {
			id.Push += "\ntest_hosts=" + strings.Join(th, ",")
		}
	}
	return state.RemoteConfigHash(id)
}

// remoteUserKeys is each allowed user as one string for the proxy's config
// hash: the bare login for an operator, and the login with tier and agents
// for a contact, so changing either restarts the proxy.
func remoteUserKeys(users []config.RemoteUser) []string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = u.Login
		if u.EffectiveTier() != config.TierOperator {
			out[i] += " tier=" + u.EffectiveTier() + " agents=" + strings.Join(u.Agents, ",")
		}
		// Only when set, so a config without see lists keeps its hash.
		if len(u.See) > 0 {
			out[i] += " see=" + strings.Join(u.See, ",")
		}
	}
	return out
}
