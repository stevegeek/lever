package config

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/stevegeek/lever/internal/voice"
)

// WhisperTool is what config load reads of a broker tool that runs
// lever-tool-whisper: a supervised tool whose command names -dictate-socket
// or -whisper-port. Values are as written ("" or 0 when absent); the tool
// checks its own flags again at start.
type WhisperTool struct {
	Name   string
	Server string // -server
	Models string // -models
	Model  string // -model ("" = the tool's default, voice.DefaultModel)
	Socket string // -dictate-socket
	// Port is -whisper-port (0 when absent or not a number; PortRaw has
	// what was written).
	Port    int
	PortRaw string
	// MaxSeconds is -max-seconds (0 when absent or not a number: the
	// tool's default, voice.DefaultMaxSeconds).
	MaxSeconds int
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

// WhisperTools lists the broker tools that run lever-tool-whisper, in
// config order.
func (a *App) WhisperTools() []WhisperTool {
	var out []WhisperTool
	for _, t := range a.Broker.Tools {
		if t.External {
			continue
		}
		w, found := WhisperTool{Name: t.Name}, false
		for _, f := range toolFlags(t) {
			switch f.name {
			case "server":
				w.Server = f.val
			case "models":
				w.Models = f.val
			case "model":
				w.Model = f.val
			case "dictate-socket":
				w.Socket, found = f.val, true
			case "whisper-port":
				w.PortRaw, found = f.val, true
				if n, err := strconv.Atoi(f.val); err == nil {
					w.Port = n
				}
			case "max-seconds":
				if n, err := strconv.Atoi(f.val); err == nil {
					w.MaxSeconds = n
				}
			}
		}
		if found {
			out = append(out, w)
		}
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
	tools := a.WhisperTools()
	seen := map[int]string{}
	for _, w := range tools {
		key := fmt.Sprintf("broker.tools[%s]", w.Name)
		if w.PortRaw == "" {
			return fmt.Errorf("config: %s -whisper-port is required: the host loopback port of its whisper-server child", key)
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
