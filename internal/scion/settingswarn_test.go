package scion

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// exitRunner errs on a scripted non-zero Code, the way a real exit arrives.
type exitRunner struct{ *proc.FakeRunner }

func (r exitRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := r.FakeRunner.RunIn(ctx, dir, env, name, args...)
	if err == nil && res.Code != 0 {
		err = errors.New("exit status 1")
	}
	return res, err
}

const settingsWarn = `2026/10/02 15:25:35 WARN settings file contains unrecognized keys (these will be ignored) keys="[hub.project_id runtimes[*].type]" path=/home/u/.scion/settings.yaml`

func TestIsSettingsKeysWarning(t *testing.T) {
	match := []string{
		settingsWarn,
		settingsWarn + "\r",
		"  " + settingsWarn,
		"\x1b[33m" + settingsWarn + "\x1b[0m",
		`WARN settings file contains unrecognized keys (these will be ignored) keys=[hub.project_id runtimes[*].type ...]`,
		`2026/10/02 15:25:35 WARN settings file contains unrecognized keys (these will be ignored)`,
	}
	for _, l := range match {
		if !IsSettingsKeysWarning(l) {
			t.Errorf("not matched: %q", l)
		}
	}
	noMatch := []string{
		"",
		`2026/10/02 15:25:35 WARN hub file contains unrecognized keys (these will be ignored) keys="[x]"`,
		`2026/10/02 15:25:35 WARN server file contains unrecognized keys (these will be ignored) keys="[x]"`,
		`2026/10/02 15:25:35 ERROR settings file contains unrecognized keys (these will be ignored) keys="[x]"`,
		`Error: settings file contains unrecognized keys (these will be ignored)`,
		`2026/10/02 15:25:35 WARN settings file contains unrecognized keys (these will be ignored); also failed`,
		`agent said: WARN settings file contains unrecognized keys (these will be ignored)`,
		`2026/10/02 15:25:35 WARN deprecated key "foo" in settings`,
	}
	for _, l := range noMatch {
		if IsSettingsKeysWarning(l) {
			t.Errorf("matched: %q", l)
		}
	}
}

// TestSettingsWarningHiddenAndLogged: the warning never reaches an error, a
// success result or a value, and each distinct line reaches the log sink
// once however many calls print it (lever#169). Other stderr text survives.
func TestSettingsWarningHiddenAndLogged(t *testing.T) {
	other := "2026/10/02 15:25:35 WARN hub file contains unrecognized keys (these will be ignored) keys=\"[x]\""
	f := proc.NewFakeRunner()
	f.Script("scion resume m", proc.Result{
		Stderr: settingsWarn + "\n" + other + "\nError: failed to start agent\n",
		Code:   1,
	})
	f.Script("scion stop m", proc.Result{Stderr: settingsWarn + "\n"})
	f.Script("scion config get default_template", proc.Result{Stdout: "default\n", Stderr: strings.Replace(settingsWarn, "15:25:35", "15:25:36", 1) + "\n"})

	resetWarnSeen()
	var logged []string
	c := New(exitRunner{f}, Options{SettingsWarnLog: func(l string) { logged = append(logged, l) }})
	ctx := context.Background()

	err := c.Resume(ctx, "m", "")
	if err == nil {
		t.Fatal("Resume: want error")
	}
	if strings.Contains(err.Error(), "settings file contains") {
		t.Errorf("error still carries the warning: %q", err)
	}
	for _, want := range []string{"hub file contains", "failed to start agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if err := c.Stop(ctx, "m", ""); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if v, err := c.ConfigGetProject(ctx, "", "default_template"); err != nil || v != "default" {
		t.Fatalf("ConfigGet = %q, %v", v, err)
	}
	if len(logged) != 1 || logged[0] != settingsWarn {
		t.Fatalf("logged %q, want the warning once", logged)
	}
}

func resetWarnSeen() {
	warnSeen.mu.Lock()
	defer warnSeen.mu.Unlock()
	warnSeen.seen = map[string]bool{}
}

// A second client in the same process does not log the line again, and a
// warning glued to stdout that lacks a final newline is still found.
func TestSettingsWarningOncePerProcess(t *testing.T) {
	resetWarnSeen()
	var logged []string
	sink := func(l string) { logged = append(logged, l) }
	f := proc.NewFakeRunner()
	f.Script("scion stop m", proc.Result{Stdout: "Stopping m", Stderr: settingsWarn + "\nError: boom\n", Code: 1})
	for range 2 {
		err := New(exitRunner{f}, Options{SettingsWarnLog: sink}).Stop(context.Background(), "m", "")
		if err == nil || strings.Contains(err.Error(), "settings file") || !strings.Contains(err.Error(), "Stopping m") {
			t.Fatalf("Stop error = %v", err)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d times, want once: %q", len(logged), logged)
	}
}

// A client without a sink still hides the warning.
func TestSettingsWarningHiddenWithoutSink(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion stop m", proc.Result{Stderr: settingsWarn + "\nError: boom\n", Code: 1})
	err := New(exitRunner{f}, Options{}).Stop(context.Background(), "m", "")
	if err == nil || strings.Contains(err.Error(), "settings file") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Stop error = %v", err)
	}
}
