package brokerctl

import (
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// ConfigHash digests the broker-relevant configuration: the broker block
// (tools, ports, knobs) and the workers list (worker specs feed the broker's
// dispatch table). apply's broker-reuse shortcut compares a running broker's
// reported hash against this to decide whether the broker must be restarted
// on re-apply (#19) — so the hash must cover exactly the config a running
// broker bakes in at start, and nothing else (a manager-image change must
// NOT bounce the broker).
// It also covers the scion block, so a declared scion change bounces the
// broker. Note what that does NOT buy: `scion.source` and `scion.binary` are
// PATHS, so replacing the artifact behind one leaves this hash byte-identical
// and a running broker survives it. Nothing here can see that. It is why the
// `--role` capability probe no longer memoises its answer (see
// scion.Client.roleFlagSupported) instead of trusting a restart to invalidate
// it — a stale "no --role" would hand every agent scion#1090's FULL default.
func ConfigHash(app *config.App) string {
	// Marshal of plain config structs cannot fail in practice; an empty hash
	// (HashJSON's failure value) makes the comparison a guaranteed mismatch
	// (restart), which fails toward the safe side.
	//
	// VerifiedChat is whether verified web chat is configured
	// (ChatConfigured) and WebSenders the remote sign-ins' sender labels:
	// both derive from the remote block, which the broker otherwise ignores,
	// so changing allowed_users must bounce the broker too.
	//
	// AgentMessages (only while on) is the contact list and limits
	// contact_message enforces; nil while off keeps the hash an instance had
	// before remote.agent_messages existed. Files likewise (share_file).
	//
	// ManagerClaude (nil when unset, the same old hash) is in the manager's
	// envelope: a change restarts the broker, which reopens the bootstrap
	// latch, so the next apply re-mints and re-stages the manager's
	// bootstrap.json with the new block. A worker's claude_settings and
	// after_compact_note are omitted while unset (their json tags on
	// config.Worker) for the same reason; a new Worker field needs the same
	// tag, or every instance with workers restarts its broker on upgrade.
	return state.HashJSON(struct {
		Broker        config.Broker
		Workers       []config.Worker
		Scion         config.ScionConfig
		VerifiedChat  bool
		WebSenders    []string            `json:",omitempty"`
		AgentMessages *agentMessagesStamp `json:",omitempty"`
		Files         *filesStamp         `json:",omitempty"`
		ManagerClaude *wire.Claude        `json:",omitempty"`
	}{app.Broker, app.Workers, app.Scion, ChatConfigured(app), WebSenders(app), agentMessagesStampOf(app), filesStampOf(app), app.ManagerClaude()})
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

// filesStamp is the part of remote.files the broker acts on (share_file):
// the limits and who may receive files from which agent.
type filesStamp struct {
	MaxBytes   int64
	Extensions []string
	Contacts   []string // "login=agent,agent", config order
	Operators  []string
	// Set only when not the default, so an instance that never sets
	// remote.files.shares or an allowed_users files key keeps its stamp.
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
// at startup — see state.RemoteConfigHash for why. This is the only place
// that maps config.App onto state.RemoteIdentity, so state stays free of
// config types.
func RemoteConfigHash(app *config.App) string {
	id := state.RemoteIdentity{
		Enabled:      app.Remote.Enabled,
		Port:         app.Remote.Port,
		BaseURL:      app.Remote.BaseURL,
		AllowedUsers: remoteUserKeys(app.Remote.AllowedUsers),
		LoginPort:    app.Remote.LoginPort,
		// Effective values, so spelling the default out (or changing the
		// header's case) is not a config change that bounces the proxy.
		IdentityHeader:     app.EffectiveRemoteIdentityHeader(),
		Bind:               app.EffectiveRemoteBind(),
		AllowWildcardBind:  app.Remote.AllowWildcardBind,
		TrustForwardedHost: app.Remote.TrustForwardedHost,
		Landing:            app.EffectiveRemoteLanding(),
		Name:               app.Name,
		Backend:            app.Backend,
		AgentMessages:      app.AgentMessagesOn(),
	}
	// The chat page's list reads the workers, the tree and the labels file;
	// only a chat-landing proxy captures them, so a console-landing proxy
	// does not restart on a worker change.
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
			id.Voice = &state.VoiceIdentity{WhisperServer: app.Remote.Voice.WhisperServer, Model: app.EffectiveVoiceModel(),
				Language: app.Remote.Voice.Language, Vocabulary: app.Remote.Voice.Vocabulary, MaxSeconds: app.EffectiveVoiceMaxSeconds(),
				GPU: app.VoiceGPU(), Port: app.EffectiveVoicePort(), Excluded: app.VoiceExcludedLogins(),
				NoReadAloud: app.Remote.Voice.ReadAloud != nil && !*app.Remote.Voice.ReadAloud}
		}
	}
	if app.PushOn() {
		id.Push = app.Remote.Push.Subject
		// The test hosts (TEST ONLY) are captured at start too; omitted when
		// unset, so the stamp of a real instance is the subject alone.
		if th := app.Remote.Push.TestHosts; len(th) > 0 {
			id.Push += "\ntest_hosts=" + strings.Join(th, ",")
		}
	}
	return state.RemoteConfigHash(id)
}

// remoteUserKeys is each allowed user as one string for the proxy's config
// hash: the bare login for an operator (so a config written before tiers
// hashes as it did), and the login with its tier and agents for a contact, so
// changing either restarts the proxy.
func remoteUserKeys(users []config.RemoteUser) []string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = u.Login
		if u.EffectiveTier() != config.TierOperator {
			out[i] += " tier=" + u.EffectiveTier() + " agents=" + strings.Join(u.Agents, ",")
		}
		// Only when set, so a config without see lists hashes as before.
		if len(u.See) > 0 {
			out[i] += " see=" + strings.Join(u.See, ",")
		}
	}
	return out
}
