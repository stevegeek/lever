package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReadAloud is remote.voice.read_aloud.
type ReadAloud string

const (
	ReadAloudDevice ReadAloud = "device"
	ReadAloudServer ReadAloud = "server"
	ReadAloudOff    ReadAloud = "off"
)

// UnmarshalYAML accepts a word (checked by validateVoice) or a bool: true is
// device and false is off.
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
	// YAML 1.1's other bool words.
	switch strings.ToLower(s) {
	case "yes", "y", "on":
		s = string(ReadAloudDevice)
	case "no", "n":
		s = string(ReadAloudOff)
	}
	*r = ReadAloud(s)
	return nil
}

func (r ReadAloud) valid() bool {
	return r == "" || r == ReadAloudDevice || r == ReadAloudServer || r == ReadAloudOff
}

// EffectiveReadAloud is off while voice is off.
func (a *App) EffectiveReadAloud() ReadAloud {
	switch {
	case !a.VoiceOn():
		return ReadAloudOff
	case a.Remote.Voice.ReadAloud == "":
		return ReadAloudDevice
	}
	return a.Remote.Voice.ReadAloud
}
