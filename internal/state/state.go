// Package state is the host-side instance state directory: the .lever-state
// layout beside lever.yaml and the file helpers host components use on it
// (JSON state, 0600 secrets, pid files, the remote-proxy stamp). It holds no
// daemon logic.
package state

import "path/filepath"

// State is the host-side instance state directory.
type State struct{ Dir string }

// DirName is the state directory's name beside lever.yaml. User-facing text
// should use it rather than a literal.
const DirName = ".lever-state"

func ForConfig(configDir string) State {
	return State{Dir: filepath.Join(configDir, DirName)}
}

func (s State) CACert() string        { return filepath.Join(s.Dir, "ca.crt") }
func (s State) CAKey() string         { return filepath.Join(s.Dir, "ca.key") }
func (s State) BrokerKey() string     { return filepath.Join(s.Dir, "broker.key") }
func (s State) BrokerPub() string     { return filepath.Join(s.Dir, "broker.pub") }
func (s State) Revocation() string    { return filepath.Join(s.Dir, "revocation.json") }
func (s State) Directives() string    { return filepath.Join(s.Dir, "directives.json") }
func (s State) DirectiveSock() string { return filepath.Join(s.Dir, "directive.sock") }
func (s State) PID() string           { return filepath.Join(s.Dir, "broker.pid") }
func (s State) Log() string           { return filepath.Join(s.Dir, "broker.log") }
func (s State) OutLog() string        { return filepath.Join(s.Dir, "broker.out.log") }
func (s State) ControllerPAT() string { return filepath.Join(s.Dir, "controller.pat") }
func (s State) SessionSecret() string { return filepath.Join(s.Dir, "session-secret") }
func (s State) RemotePAT() string     { return filepath.Join(s.Dir, "remote.pat") }
func (s State) RemotePID() string     { return filepath.Join(s.Dir, "remote.pid") }
func (s State) RemoteLog() string     { return filepath.Join(s.Dir, "remote.log") }
func (s State) RemoteAudit() string   { return filepath.Join(s.Dir, "remote-audit.jsonl") }

// ChatLedger is the remote proxy's record of verified web chat posts
// (chatledger), one file per login: written by the proxy only, read by the
// broker only.
func (s State) ChatLedger() string { return filepath.Join(s.Dir, "chat-ledger") }

// ChatVerified is the broker's record of chat messages already verified (one
// use each), kept across broker restarts.
func (s State) ChatVerified() string { return filepath.Join(s.Dir, "chat-verified.jsonl") }

// SentLedger is the broker's record of every message lever sends to an agent
// (sentledger): written and read by the broker only.
func (s State) SentLedger() string { return filepath.Join(s.Dir, "sent-ledger") }

// AgentLedger is the broker's record of messages agents were authorized to
// send to contacts (agentledger): written and read by the broker only.
func (s State) AgentLedger() string { return filepath.Join(s.Dir, "agent-ledger") }

// FilesLedger is the record of the chat page's file exchange (package
// fileledger): uploads written by the remote proxy, shares by the broker.
func (s State) FilesLedger() string { return filepath.Join(s.Dir, "files-ledger") }

// VoiceModels holds the pinned Whisper models `lever voice fetch` downloads
// (0700, each file 0600); a tool whose -models points here checks the model
// before each whisper-server start. Never audio or a transcript.
func (s State) VoiceModels() string { return filepath.Join(s.Dir, "voice-models") }

// Sessions records each agent's last fresh session start and the skill text
// then (sessionrec): written by apply (manager) and the broker (workers),
// read by the remote proxy before it lets a contact post to an agent.
func (s State) Sessions() string { return filepath.Join(s.Dir, "sessions.jsonl") }

// ConversationLock is the flock every change to the manager's record takes
// (lever conversation new and restore, up, apply, stop, reload, and the
// broker's heal of the manager).
func (s State) ConversationLock() string { return filepath.Join(s.Dir, "conversation.lock") }

// OperatorSock is the broker's UNIX socket for `lever msg send` (0600,
// bound only when the state directory is outside the tree).
func (s State) OperatorSock() string { return filepath.Join(s.Dir, "operator.sock") }

// DirectiveAudit is the broker's operator-directive audit log.
func (s State) DirectiveAudit() string { return filepath.Join(s.Dir, "directives.log") }

// Skills records the scaffold hashes `lever init` wrote into the tree;
// SkillsAdopted records owner-blessed customizations (`init --adopt`).
func (s State) Skills() string        { return filepath.Join(s.Dir, "skills.json") }
func (s State) SkillsAdopted() string { return filepath.Join(s.Dir, "skills-adopted.json") }

// RemoteStamp records the version and remote config a running proxy was
// started with, so apply can tell "already serving" from "serving something
// else". The proxy must not grow an HTTP endpoint for this (unlike the
// broker's /epoch): it fronts the hub, so any listener of its own would be
// reachable by whatever reaches the proxy. A host-side file only lever
// writes keeps the answer private.
func (s State) RemoteStamp() string { return filepath.Join(s.Dir, "remote.stamp") }

// GuestBoot records the jail guest boot id in which lever last saw the
// manager live, so a VM restart under a "running" record is recognised.
func (s State) GuestBoot() string { return filepath.Join(s.Dir, "guest-boot.json") }

// HWEFailStamp records that the guest_kernel hwe install failed in the jail
// VM, keyed by its /etc/machine-id: while it names the VM and egress is
// closed, apply and up warn instead of running apt again. Deleting it
// retries.
func (s State) HWEFailStamp() string { return filepath.Join(s.Dir, "hwe-install-failed.json") }

// GuestLogs holds the jail VM's console logs, saved before each VM start
// and when a running VM's console shows a kernel panic (0700, each file 0600).
func (s State) GuestLogs() string { return filepath.Join(s.Dir, "guest-logs") }

// PushDir holds the remote proxy's Web Push key, subscriptions and last
// send result (remote.push), each 0600.
func (s State) PushDir() string { return filepath.Join(s.Dir, "push") }

func (s State) ToolLogDir() string { return filepath.Join(s.Dir, "tool-logs") }

// BringupLogDir holds one JSON-lines record per bring-up, and LastBringup.
func (s State) BringupLogDir() string { return filepath.Join(s.Dir, "logs") }

// LastBringup is the summary of the last bring-up, which doctor shows.
func (s State) LastBringup() string { return filepath.Join(s.BringupLogDir(), "last-bringup.json") }
