package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/stevegeek/lever/internal/voice"
)

// WhisperTool is what config load reads of a broker tool that runs
// lever-tool-whisper: a supervised tool whose program, after an env(1)
// prefix, has the base name lever-tool-whisper (isWhisperCommand). Values
// are as written ("" or 0 when absent); the tool checks its own flags again
// at start.
type WhisperTool struct {
	Name   string
	Tree   string // -tree
	Server string // -server
	Models string // -models
	Model  string // -model ("" = the tool's default, voice.DefaultModel)
	Socket string // -dictate-socket
	// Port is -whisper-port (0 when absent or not a number; PortRaw has
	// what was written).
	Port    int
	PortRaw string
	// MaxSeconds is -max-seconds (0 when absent or not a number: the
	// tool's default, voice.DefaultMaxSeconds); MaxSecondsSet says the flag
	// is there, MaxSecondsRaw has what was written.
	MaxSeconds    int
	MaxSecondsSet bool
	MaxSecondsRaw string
	// AgentMaxSeconds is -agent-max-seconds, likewise (0: the tool's
	// default, voice.DefaultAgentMaxSeconds within -max-seconds).
	AgentMaxSeconds    int
	AgentMaxSecondsSet bool
	AgentMaxSecondsRaw string
}

// EffectiveModel is -model with the tool's default.
func (w WhisperTool) EffectiveModel() string {
	if w.Model == "" {
		return voice.DefaultModel
	}
	return w.Model
}

// EffectiveMaxSeconds is -max-seconds with the tool's default.
func (w WhisperTool) EffectiveMaxSeconds() int {
	if w.MaxSeconds <= 0 {
		return voice.DefaultMaxSeconds
	}
	return w.MaxSeconds
}

// EffectiveAgentMaxSeconds is -agent-max-seconds with the tool's default
// (voice.AgentMaxSeconds).
func (w WhisperTool) EffectiveAgentMaxSeconds() int {
	return voice.AgentMaxSeconds(w.AgentMaxSeconds, w.EffectiveMaxSeconds())
}

// WhisperTools lists the broker tools that run lever-tool-whisper, in
// config order: the program's base name (after an env prefix) is
// lever-tool-whisper, or the command takes -dictate-socket (isWhisperCommand).
func (a *App) WhisperTools() []WhisperTool {
	var out []WhisperTool
	for _, t := range a.Broker.Tools {
		if !isWhisperCommand(t) {
			continue
		}
		w := WhisperTool{Name: t.Name}
		for _, f := range toolFlags(t) {
			switch f.name {
			case "tree":
				w.Tree = f.val
			case "server":
				w.Server = f.val
			case "models":
				w.Models = f.val
			case "model":
				w.Model = f.val
			case "dictate-socket":
				w.Socket = f.val
			case "whisper-port":
				w.PortRaw, w.Port = f.val, 0
				if n, err := strconv.Atoi(f.val); err == nil {
					w.Port = n
				}
			case "max-seconds":
				w.MaxSecondsSet, w.MaxSecondsRaw, w.MaxSeconds = true, f.val, 0
				if n, err := strconv.Atoi(f.val); err == nil {
					w.MaxSeconds = n
				}
			case "agent-max-seconds":
				w.AgentMaxSecondsSet, w.AgentMaxSecondsRaw, w.AgentMaxSeconds = true, f.val, 0
				if n, err := strconv.Atoi(f.val); err == nil {
					w.AgentMaxSeconds = n
				}
			}
		}
		out = append(out, w)
	}
	return out
}

// VoiceTool is the whisper tool whose -dictate-socket is remote.voice.socket
// (dictation's tool), when remote.voice is on and there is one.
func (a *App) VoiceTool() (WhisperTool, bool) {
	if !a.VoiceOn() {
		return WhisperTool{}, false
	}
	for _, w := range a.WhisperTools() {
		if w.Socket != "" && w.Socket == a.Remote.Voice.Socket {
			return w, true
		}
	}
	return WhisperTool{}, false
}

// validateWhisperTools checks what config load can of each whisper tool's
// command. The port matters most: whisper-server has no authentication of
// its own, and it is safe only because it listens on host loopback and the
// jail's egress rules drop every host-loopback port that is not
// allowlisted. So -whisper-port must never be a port the jail may reach
// (EffectiveAllowedPorts: the broker's jail port, manager.allow_ports, the
// login port), nor collide with one of lever's own host listeners or a tool
// backend.
func (a *App) validateWhisperTools() error {
	for _, t := range a.Broker.Tools {
		if form := envFormUnread(t.Command); form != "" {
			return fmt.Errorf("config: broker.tools[%s] command uses %s, which lever cannot read the way env does, so its flags would escape the host-path checks; write the command as separate words (env NAME=value program -flag value)", t.Name, form)
		}
	}
	tools := a.WhisperTools()
	seen := map[int]string{}
	for _, w := range tools {
		key := fmt.Sprintf("broker.tools[%s]", w.Name)
		if w.PortRaw == "" {
			return fmt.Errorf("config: %s -whisper-port is required: the host loopback port of its whisper-server child (a tool that runs lever-tool-whisper, or takes -dictate-socket, is checked as the whisper tool)", key)
		}
		if w.Port < 1 || w.Port > 65535 {
			return fmt.Errorf("config: %s -whisper-port %q is not a port", key, w.PortRaw)
		}
		if w.Socket == "" {
			return fmt.Errorf("config: %s -dictate-socket is required: the absolute path of its dictation socket, outside the tree", key)
		}
		if err := voice.CheckSocketPath(w.Socket); err != nil {
			return fmt.Errorf("config: %s -dictate-socket: %w", key, err)
		}
		if w.Models == "" || w.Server == "" {
			return fmt.Errorf("config: %s needs -models and -server (absolute host paths outside the tree)", key)
		}
		if w.MaxSecondsSet {
			n, err := strconv.Atoi(w.MaxSecondsRaw)
			if err != nil {
				return fmt.Errorf("config: %s -max-seconds %q is not a number of seconds", key, w.MaxSecondsRaw)
			}
			if err := voice.CheckMaxSeconds(n); err != nil {
				return fmt.Errorf("config: %s -max-seconds: %w", key, err)
			}
		}
		if w.AgentMaxSecondsSet {
			n, err := strconv.Atoi(w.AgentMaxSecondsRaw)
			if err != nil {
				return fmt.Errorf("config: %s -agent-max-seconds %q is not a number of seconds", key, w.AgentMaxSecondsRaw)
			}
			if err := voice.CheckAgentMaxSeconds(n, w.EffectiveMaxSeconds()); err != nil {
				return fmt.Errorf("config: %s -agent-max-seconds: %w", key, err)
			}
		}
		// The tool checks its paths against -tree and reads agent files
		// below it: it must be this instance's tree.
		if w.Tree == "" {
			return fmt.Errorf("config: %s -tree is required: the instance's tree, %s", key, a.Tree)
		}
		if !sameTree(w.Tree, a.Tree) {
			return fmt.Errorf("config: %s -tree %s is not the instance's tree %s", key, w.Tree, a.Tree)
		}
		if other, dup := seen[w.Port]; dup {
			return fmt.Errorf("config: %s -whisper-port %d is also broker tool %q's", key, w.Port, other)
		}
		seen[w.Port] = w.Name
		if slices.Contains(a.EffectiveAllowedPorts(), w.Port) {
			return fmt.Errorf("config: %s -whisper-port %d is a port the jail may reach (manager.allow_ports, the broker's jail port or the login port): "+
				"whisper-server has no authentication, and only the egress rules keep agents from it; pick another port, or remove it from manager.allow_ports", key, w.Port)
		}
		own := []struct {
			port int
			what string
		}{
			{a.EffectiveAdminPort(), "the broker's admin port"},
			{GuestLoginIssuerPort, "the port the jail's login forwarder is mirrored onto"},
		}
		if a.RemoteEnabled() {
			own = append(own, struct {
				port int
				what string
			}{a.EffectiveRemotePort(), "the remote proxy's port"})
		}
		for _, o := range own {
			if w.Port == o.port {
				return fmt.Errorf("config: %s -whisper-port %d collides with %s; pick another", key, w.Port, o.what)
			}
		}
		for _, t := range a.Broker.Tools {
			if port, ok := backendPort(t.Backend); ok && port == w.Port {
				return fmt.Errorf("config: %s -whisper-port %d collides with broker tool %q's backend port; pick another", key, w.Port, t.Name)
			}
		}
	}
	for i, w := range tools {
		for _, o := range tools[i+1:] {
			if w.Socket == o.Socket {
				return fmt.Errorf("config: broker tools %q and %q name the same -dictate-socket %s", w.Name, o.Name, w.Socket)
			}
		}
	}
	return nil
}

// sameTree reports whether p names the tree: the same clean path, or the
// same path once symbolic links are followed.
func sameTree(p, tree string) bool {
	if filepath.Clean(p) == filepath.Clean(tree) {
		return true
	}
	rp, err1 := resolveExisting(p)
	rt, err2 := resolveExisting(tree)
	return err1 == nil && err2 == nil && rp == rt
}
