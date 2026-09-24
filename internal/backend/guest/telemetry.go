package guest

import (
	"bytes"
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

// Convergence of the top-level `telemetry:` block in the jail's
// ~/.scion/settings.yaml (config scion.telemetry).
//
// Why lever writes it at all: since scion#1792 sciontool telemetry defaults ON
// (pkg/sciontool/telemetry/config.go LoadConfig, SCION_TELEMETRY_ENABLED
// default true), and so does cloud export. With no cloud destination
// configured, the pipeline `sciontool init` starts refuses to start
// ("cloud telemetry enabled but destination is not configured",
// pkg/sciontool/telemetry/pipeline.go Start), so nothing listens on the
// in-container 127.0.0.1:4317 — yet every `sciontool hook` invocation still
// builds OTLP exporters against it (cmd/sciontool/commands/hook.go) and waits
// out their export and shutdown timeouts. Claude fires 8 hook events,
// including Pre/PostToolUse on every tool call, so a trivial turn took
// minutes.
//
// `telemetry.enabled: false` is what scion turns into
// SCION_TELEMETRY_ENABLED=false for every agent it starts
// (pkg/config/telemetry_convert.go TelemetryConfigToEnv). `scion config set`
// refuses the key ("unknown or complex setting key"), hence the file edit.
//
// It takes effect at agent START: the runtime broker reads the file on every
// start (pkg/agent/run.go LoadEffectiveSettings), but a running container keeps
// the env it was created with, and the hub caches the block at its own startup
// as the default it stamps onto newly created agents. So a change reported
// here is acted on by `lever stop && lever up`, which the caller tells the
// operator — lever does not restart the manager on its own.

// EnsureScionTelemetry converges the jail's settings file on the
// posture off asks for, and reports whether it wrote the file.
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
	if err != nil || !changed {
		return false, err
	}
	if err := g.writeScionSettings(ctx, updated); err != nil {
		return false, err
	}
	return true, nil
}

// telemetrySettingsConverged returns the settings content for the requested
// telemetry posture, and whether it differs from existing.
//
//   - off: `telemetry.enabled` is set to false. Every other key in the block
//     (an operator's cloud/filter config) and in the file is kept; an explicit
//     `enabled: true` is overridden, because lever's config is the switch — the
//     way to keep scion's telemetry on is scion.telemetry: scion-default.
//   - !off (scion-default): the block is removed ONLY when it is exactly what
//     off writes (`enabled: false` and nothing else). Anything richer is the
//     operator's own and stays untouched.
//
// "Changed" is semantic, so a re-apply that finds the file converged writes
// nothing. A `telemetry:` that is not a mapping is refused on the off path
// (lever cannot set a key inside it) and left alone on the other.
func telemetrySettingsConverged(existing []byte, off bool) ([]byte, bool, error) {
	if !off && len(bytes.TrimSpace(existing)) == 0 {
		return existing, false, nil
	}
	doc, err := layout.ParseSettings(existing)
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

// isLeverTelemetryOff reports whether a telemetry node is exactly the block
// the off path writes into a file that had none.
func isLeverTelemetryOff(n *yaml.Node) bool {
	return n.Kind == yaml.MappingNode && len(n.Content) == 2 &&
		n.Content[0].Value == layout.KeyEnabled && isFalse(n.Content[1])
}

// isFalse reports whether a scalar node decodes to boolean false.
func isFalse(n *yaml.Node) bool {
	var b bool
	return n.Kind == yaml.ScalarNode && n.Decode(&b) == nil && !b
}

// ScionTelemetryEnabled reads what the settings file says about agent
// telemetry: "false" / "true" for an explicit `telemetry.enabled`, "" when the
// key is absent (scion's default, which is ON). Pure; used by doctor.
func ScionTelemetryEnabled(settings []byte) (string, error) {
	doc, err := layout.ParseSettings(settings)
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
