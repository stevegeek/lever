package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ReadAloud is remote.voice.read_aloud.
type ReadAloud string

// The remote.voice.read_aloud values.
const (
	ReadAloudDevice ReadAloud = "device"
	ReadAloudServer ReadAloud = "server"
	ReadAloudOff    ReadAloud = "off"
)

// UnmarshalYAML accepts a word (checked by validateVoice) or a bool: true
// is device and false is off, as read_aloud was a bool before server.
func (r *ReadAloud) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("config: remote: voice.read_aloud is device, server or off (line %d)", n.Line)
	}
	if n.Tag == "!!bool" {
		var b bool
		if err := n.Decode(&b); err != nil {
			return err
		}
		*r = ReadAloudOff
		if b {
			*r = ReadAloudDevice
		}
		return nil
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	*r = ReadAloud(s)
	return nil
}

// valid reports whether r is a read_aloud word ("" is the default).
func (r ReadAloud) valid() bool {
	return r == "" || r == ReadAloudDevice || r == ReadAloudServer || r == ReadAloudOff
}

// EffectiveReadAloud is remote.voice.read_aloud as the chat page gets it:
// its value, device when unset, and off while voice is off.
func (a *App) EffectiveReadAloud() ReadAloud {
	switch {
	case !a.VoiceOn():
		return ReadAloudOff
	case a.Remote.Voice.ReadAloud == "":
		return ReadAloudDevice
	}
	return a.Remote.Voice.ReadAloud
}
