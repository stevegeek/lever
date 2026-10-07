package config

import (
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

const claudeBase = "name: demo\nbackend: orbstack\ntree: ws\n"

func TestClaudeSettingsLoadAndNotInherited(t *testing.T) {
	body := claudeBase +
		"manager:\n  claude_settings:\n    auto_compact_window: 400000\n  after_compact_note: Re-read NOTES.md before you go on.\n" +
		"workers:\n  - name: w\n    dir: workers/w\n    claude_settings: {auto_compact_window: 200000}\n  - name: bare\n    dir: workers/bare\n"
	app, err := LoadNoHostChecks(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := app.ManagerClaude(), (&wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "Re-read NOTES.md before you go on."}); *got != *want {
		t.Fatalf("ManagerClaude = %+v, want %+v", got, want)
	}
	if got := app.Workers[0].Claude(); got == nil || *got != (wire.Claude{AutoCompactWindow: 200000}) {
		t.Fatalf("worker w Claude = %+v, want window 200000 and no note", got)
	}
	// Not inherited: a worker without its own config gets none.
	if got := app.Workers[1].Claude(); got != nil {
		t.Fatalf("worker bare Claude = %+v, want nil", got)
	}
}

func TestClaudeSettingsUnsetIsNil(t *testing.T) {
	app, err := LoadNoHostChecks(writeConfig(t, claudeBase+"manager: {}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := app.ManagerClaude(); got != nil {
		t.Fatalf("ManagerClaude = %+v, want nil when nothing is set", got)
	}
}

func TestClaudeSettingsRejected(t *testing.T) {
	long := strings.Repeat("x", wire.MaxAfterCompactNoteBytes+1)
	cases := map[string]struct{ body, want string }{
		"unknown key":          {claudeBase + "manager:\n  claude_settings:\n    autoCompactWindow: 400000\n", `unknown key "autoCompactWindow"`},
		"not a mapping":        {claudeBase + "manager:\n  claude_settings: 400000\n", "claude_settings must be a mapping"},
		"window too small":     {claudeBase + "manager:\n  claude_settings: {auto_compact_window: 99999}\n", "auto_compact_window must be between"},
		"window too large":     {claudeBase + "manager:\n  claude_settings: {auto_compact_window: 1000001}\n", "auto_compact_window must be between"},
		"negative window":      {claudeBase + "manager:\n  claude_settings: {auto_compact_window: -1}\n", "auto_compact_window must be between"},
		"worker unknown key":   {claudeBase + "manager: {}\nworkers:\n  - name: w\n    dir: workers/w\n    claude_settings: {model: opus}\n", `unknown key "model"`},
		"worker window":        {claudeBase + "manager: {}\nworkers:\n  - name: w\n    dir: workers/w\n    claude_settings: {auto_compact_window: 5}\n", `worker "w"`},
		"note with newline":    {claudeBase + "manager:\n  after_compact_note: \"one\\ntwo\"\n", "after_compact_note must be one line"},
		"note with escape":     {claudeBase + "manager:\n  after_compact_note: \"a\\x1b[2Jb\"\n", "after_compact_note must be one line"},
		"note with bidi":       {claudeBase + "manager:\n  after_compact_note: \"a\\u202eb\"\n", "after_compact_note must be one line"},
		"note too long":        {claudeBase + "manager:\n  after_compact_note: " + long + "\n", "after_compact_note must be at most"},
		"worker note too long": {claudeBase + "manager: {}\nworkers:\n  - name: w\n    dir: workers/w\n    after_compact_note: " + long + "\n", "after_compact_note must be at most"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) { rejectNoHost(t, c.body, c.want) })
	}
}
