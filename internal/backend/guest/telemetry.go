package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

// The top-level `telemetry:` block in the jail's ~/.scion/settings.yaml.
//
// sciontool telemetry and cloud export default ON. With no cloud destination
// the pipeline refuses to start, so nothing listens on 127.0.0.1:4317, yet
// every `sciontool hook` still builds OTLP exporters against it and waits out
// their timeouts; Claude fires hooks on every tool call, so a trivial turn
// takes minutes.
//
// scion turns `telemetry.enabled: false` into SCION_TELEMETRY_ENABLED=false
// for every agent it starts. `scion config set` refuses the key, hence the
// file edit. It takes effect only at agent START (and the hub caches the
// block at its own startup), so the caller tells the operator to stop and
// up; lever does not restart the manager on its own.

// EnsureScionTelemetry reports whether it changed the telemetry block. The
// same write removes auto_inject_gcloud_adc (see withoutGcloudADC).
func (g Guest) EnsureScionTelemetry(ctx context.Context, off bool) (bool, error) {
	res, err := g.UserRun(ctx, "/bin/bash", "-c", readScionSettingsScript)
	if err != nil {
		// Fatal, like ensureHubLoginSettings: treating an unreadable file as
		// absent would rewrite scion's machine configuration from nothing.
		return false, fmt.Errorf("guest: read %s: %w", layout.SettingsRel, err)
	}
	existing, _, err := parseScionSettingsRead(res.Stdout)
	if err != nil {
		return false, err
	}
	updated, changed, err := telemetrySettingsConverged(existing, off)
	if err != nil {
		return false, err
	}
	updated, removed, err := withoutGcloudADC(updated)
	if err != nil || !changed && !removed {
		return false, err
	}
	if err := g.writeScionSettings(ctx, updated); err != nil {
		return false, err
	}
	return changed, nil
}

// keyGcloudADC is the settings key that makes scion's broker copy the jail
// user's gcloud application-default credentials into every agent it starts.
// Any loopback caller, an agent included, can set it through the workstation
// settings route, and the broker reads it at each start. lever never sets it,
// so every apply removes it.
const keyGcloudADC = "auto_inject_gcloud_adc"

func withoutGcloudADC(existing []byte) ([]byte, bool, error) {
	if !bytes.Contains(existing, []byte(keyGcloudADC)) {
		return existing, false, nil
	}
	doc, err := parseJailSettings(existing)
	if err != nil {
		return nil, false, fmt.Errorf("guest: parse the jail's %s: %w", layout.SettingsRel, err)
	}
	root := layout.DocumentRoot(doc)
	if root == nil || layout.MapGet(root, keyGcloudADC) == nil {
		return existing, false, nil
	}
	layout.MapDelete(root, keyGcloudADC)
	out, err := encodeSettings(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// telemetrySettingsConverged:
//
//   - off: `telemetry.enabled` is set to false, keeping every other key; an
//     explicit `enabled: true` is overridden, because lever's config is the
//     switch.
//   - !off: the block is removed ONLY when it is exactly what off writes.
//     Anything richer is the operator's own.
//
// "Changed" is semantic, so a converged file is not rewritten.
func telemetrySettingsConverged(existing []byte, off bool) ([]byte, bool, error) {
	if !off && len(bytes.TrimSpace(existing)) == 0 {
		return existing, false, nil
	}
	doc, err := parseJailSettings(existing)
	if err != nil {
		return nil, false, fmt.Errorf("guest: parse the jail's %s: %w", layout.SettingsRel, err)
	}
	root := layout.DocumentRoot(doc)
	if root == nil {
		if !off {
			return existing, false, nil
		}
		return nil, false, fmt.Errorf("guest: the jail's %s is not a YAML mapping", layout.SettingsRel)
	}
	tel := layout.MapGet(root, layout.KeyTelemetry)
	if !off {
		if tel == nil || !isLeverTelemetryOff(tel) {
			return existing, false, nil
		}
		layout.MapDelete(root, layout.KeyTelemetry)
		out, err := encodeSettings(doc)
		return out, err == nil, err
	}
	if tel == nil {
		tel = layout.NewMapping()
		layout.MapSet(root, layout.KeyTelemetry, tel)
	}
	if tel.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("guest: `telemetry:` in the jail's %s is not a mapping — fix it by hand, or set scion.telemetry: scion-default", layout.SettingsRel)
	}
	if cur := layout.MapGet(tel, layout.KeyEnabled); cur != nil && isFalse(cur) {
		return existing, false, nil
	}
	layout.MapSet(tel, layout.KeyEnabled, layout.BoolNode(false))
	out, err := encodeSettings(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func isLeverTelemetryOff(n *yaml.Node) bool {
	return n.Kind == yaml.MappingNode && len(n.Content) == 2 &&
		n.Content[0].Value == layout.KeyEnabled && isFalse(n.Content[1])
}

func isFalse(n *yaml.Node) bool {
	var b bool
	return n.Kind == yaml.ScalarNode && n.Decode(&b) == nil && !b
}

const settingsErrMax = 200

// parseJailSettings is for a settings file the guest wrote: a YAML error can
// quote the text it failed on (an alias quotes its whole anchor name).
func parseJailSettings(content []byte) (*yaml.Node, error) {
	doc, err := layout.ParseSettings(content)
	if err != nil {
		return nil, errors.New(SafeLine(err.Error(), settingsErrMax))
	}
	return doc, nil
}

// ScionTelemetryEnabled returns "false" / "true" for an explicit
// `telemetry.enabled`, "" when the key is absent (scion's default, which is
// ON).
func ScionTelemetryEnabled(settings []byte) (string, error) {
	doc, err := parseJailSettings(settings)
	if err != nil {
		return "", err
	}
	root := layout.DocumentRoot(doc)
	if root == nil {
		return "", fmt.Errorf("not a YAML mapping")
	}
	tel := layout.MapGet(root, layout.KeyTelemetry)
	if tel == nil || tel.Kind != yaml.MappingNode {
		return "", nil
	}
	en := layout.MapGet(tel, layout.KeyEnabled)
	if en == nil || en.Kind != yaml.ScalarNode {
		return "", nil
	}
	var b bool
	if err := en.Decode(&b); err != nil {
		return "", fmt.Errorf("telemetry.enabled %q is not a boolean", en.Value)
	}
	return fmt.Sprint(b), nil
}
