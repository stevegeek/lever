// Package state is the host-side instance state directory: the .lever-state
// layout beside lever.yaml and the file helpers every host-side component
// (brokerctl, remoteproxy, the cli) reads and writes it with — JSON state,
// 0600 secrets, pid files and the remote-proxy stamp. It holds no daemon
// logic of its own.
package state

import "path/filepath"

// State is the host-side instance state directory (keys, revocation, pids,
// logs, secrets).
type State struct{ Dir string }

// ForConfig returns the .lever-state directory beside the config.
func ForConfig(configDir string) State {
	return State{Dir: filepath.Join(configDir, ".lever-state")}
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

// DirectiveAudit is the broker's operator-directive audit log.
func (s State) DirectiveAudit() string { return filepath.Join(s.Dir, "directives.log") }

// Skills records the scaffold hashes `lever init` wrote into the tree;
// SkillsAdopted records owner-blessed customizations (`init --adopt`).
func (s State) Skills() string        { return filepath.Join(s.Dir, "skills.json") }
func (s State) SkillsAdopted() string { return filepath.Join(s.Dir, "skills-adopted.json") }

// RemoteStamp records the binary version + remote config a RUNNING proxy was
// started with, so apply can tell "already serving" from "serving something
// else". The broker answers that question over HTTP (/epoch); the proxy has no
// such endpoint and must not grow one — it fronts the hub, so any listener of
// its own would be reachable by whatever reaches the proxy. A file beside
// remote.pid keeps the answer host-side, where only lever writes it.
func (s State) RemoteStamp() string { return filepath.Join(s.Dir, "remote.stamp") }

// ToolLogDir is the directory holding per-supervised-tool logs.
func (s State) ToolLogDir() string { return filepath.Join(s.Dir, "tool-logs") }
