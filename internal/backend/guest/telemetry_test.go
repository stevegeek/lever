package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// telemetryBlock digs out the top-level telemetry mapping, or nil.
func telemetryBlock(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m := unmarshalSettings(t, b)
	block, _ := m["telemetry"].(map[string]any)
	return block
}

func TestTelemetrySettingsOffWritesEnabledFalse(t *testing.T) {
	for name, existing := range map[string]string{
		"absent file": "",
		"no block":    "version: 1\nserver:\n  hub:\n    port: 8080\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, err := telemetrySettingsConverged([]byte(existing), true)
			if err != nil {
				t.Fatalf("telemetrySettingsConverged: %v", err)
			}
			if !changed {
				t.Fatal("changed = false, but the file had no telemetry.enabled")
			}
			if b := telemetryBlock(t, out); b == nil || b["enabled"] != false {
				t.Fatalf("telemetry block = %v, want enabled: false\n%s", b, out)
			}
			// Top level, never under server: — scion reads it as a sibling
			// (pkg/config/hub_config.go, pkg/agent/run.go settings.Telemetry).
			if server, ok := unmarshalSettings(t, out)["server"].(map[string]any); ok {
				if _, bad := server["telemetry"]; bad {
					t.Fatalf("telemetry written under server:\n%s", out)
				}
			}
		})
	}
}

func TestTelemetrySettingsOffPreservesEverythingElse(t *testing.T) {
	existing := []byte(`# hand annotated
version: 1
server:
  user_access_mode: open
telemetry:
  # an operator's own filter config
  filter:
    enabled: true
  enabled: true
`)
	out, changed, err := telemetrySettingsConverged(existing, true)
	if err != nil {
		t.Fatalf("telemetrySettingsConverged: %v", err)
	}
	if !changed {
		t.Fatal("an explicit enabled: true was not overridden — scion.telemetry off is the switch")
	}
	b := telemetryBlock(t, out)
	if b["enabled"] != false {
		t.Fatalf("enabled = %v, want false\n%s", b["enabled"], out)
	}
	if f, ok := b["filter"].(map[string]any); !ok || f["enabled"] != true {
		t.Fatalf("the operator's filter block was lost:\n%s", out)
	}
	for _, keep := range []string{"# hand annotated", "user_access_mode: open", "# an operator's own filter config"} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("lost %q:\n%s", keep, out)
		}
	}
}

func TestTelemetrySettingsOffIsIdempotent(t *testing.T) {
	first, _, err := telemetrySettingsConverged([]byte("version: 1\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := telemetrySettingsConverged(first, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(out) != string(first) {
		t.Fatalf("a converged file reported a change / was rewritten:\n%s", out)
	}
}

func TestTelemetrySettingsOffRefusesANonMappingBlock(t *testing.T) {
	if _, _, err := telemetrySettingsConverged([]byte("telemetry: false\n"), true); err == nil {
		t.Fatal("a scalar telemetry: was silently replaced")
	}
}

func TestTelemetrySettingsScionDefaultRemovesOnlyLeversBlock(t *testing.T) {
	leverWrote, _, err := telemetrySettingsConverged([]byte("version: 1\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := telemetrySettingsConverged(leverWrote, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || telemetryBlock(t, out) != nil || !strings.Contains(string(out), "version: 1") {
		t.Fatalf("scion-default did not remove exactly lever's block (changed=%v):\n%s", changed, out)
	}

	for name, existing := range map[string]string{
		"operator's richer block": "telemetry:\n  enabled: false\n  filter:\n    enabled: true\n",
		"operator enabled it":     "telemetry:\n  enabled: true\n",
		"no block":                "version: 1\n",
		"empty file":              "",
		"scalar block":            "telemetry: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, err := telemetrySettingsConverged([]byte(existing), false)
			if err != nil {
				t.Fatalf("telemetrySettingsConverged: %v", err)
			}
			if changed || string(out) != existing {
				t.Fatalf("scion-default touched a block lever did not write:\n%s", out)
			}
		})
	}
}

func TestScionTelemetryEnabled(t *testing.T) {
	for existing, want := range map[string]string{
		"":                               "",
		"version: 1\n":                   "",
		"telemetry:\n  enabled: false\n": "false",
		"telemetry:\n  enabled: true\n":  "true",
		"telemetry:\n  filter: {}\n":     "",
	} {
		got, err := ScionTelemetryEnabled([]byte(existing))
		if err != nil || got != want {
			t.Fatalf("ScionTelemetryEnabled(%q) = %q, %v; want %q", existing, got, err, want)
		}
	}
	if _, err := ScionTelemetryEnabled([]byte("telemetry:\n  enabled: maybe\n")); err == nil {
		t.Fatal("a non-boolean enabled was read as a setting")
	}
}

// TestEnsureScionTelemetryWritesOnlyOnChange composes the read, the pure
// convergence and the write through the guest transport.
func TestEnsureScionTelemetryWritesOnlyOnChange(t *testing.T) {
	run := func(t *testing.T, settings string, off bool) (bool, string, int) {
		t.Helper()
		f := proc.NewFakeRunner()
		f.Script("orb -m m /bin/bash -c", proc.Result{Stdout: "LEGACY 0\n" + settings})
		f.Script("orb -m m bash -c", proc.Result{})
		g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}
		changed, err := g.EnsureScionTelemetry(context.Background(), off)
		if err != nil {
			t.Fatalf("EnsureScionTelemetry: %v", err)
		}
		written, writes := writtenSettings(f)
		return changed, written, writes
	}

	changed, written, writes := run(t, "version: 1\n", true)
	if !changed || writes != 1 || !strings.Contains(written, "enabled: false") {
		t.Fatalf("off on a bare file: changed=%v writes=%d\n%s", changed, writes, written)
	}
	if changed, _, writes := run(t, written, true); changed || writes != 0 {
		t.Fatalf("a converged file was rewritten: changed=%v writes=%d", changed, writes)
	}
	if changed, _, writes := run(t, "version: 1\n", false); changed || writes != 0 {
		t.Fatalf("scion-default on a file without lever's block wrote: changed=%v writes=%d", changed, writes)
	}
}

func TestEnsureScionTelemetryFailsWhenItCannotRead(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -m m /bin/bash -c", proc.Result{Stdout: "not the LEGACY header\n"})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}}
	if _, err := g.EnsureScionTelemetry(context.Background(), true); err == nil {
		t.Fatal("an unreadable settings file was treated as empty")
	}
}
