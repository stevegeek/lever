package config

import (
	"strings"
	"testing"
)

// With a whisper or github tool configured, no worker dir may overlap the
// tree root's .lever-files, whether remote.files is on or not.
func TestWorkerDirOverlappingLeverFilesRefusedWithTools(t *testing.T) {
	github := Tool{Name: "gh", Command: []string{"/opt/lever/lever-tool-github", "-tree", "/t"}, Backend: "127.0.0.1:3300"}
	githubByKey := Tool{Name: "gh", Command: []string{"env", "A=1", "/opt/x/pusher", "-app-key", "/s/key.pem"}, Backend: "127.0.0.1:3300"}
	githubByName := Tool{Name: "github", External: true, Backend: "127.0.0.1:3300"}
	db := Tool{Name: "db", Command: []string{"/opt/lever/lever-tool-db"}, Backend: "127.0.0.1:3301"}
	for _, tc := range []struct {
		name  string
		tools []Tool
		dir   string
		bad   bool
	}{
		{"whisper, dir is .lever-files", []Tool{whisperTool()}, ".lever-files", true},
		{"whisper, dir inside it", []Tool{whisperTool()}, ".lever-files/whisper", true},
		{"whisper, case-folded", []Tool{whisperTool()}, ".Lever-Files/Whisper", true},
		{"github, dir inside it", []Tool{github}, ".lever-files/github", true},
		{"github by -app-key behind env", []Tool{githubByKey}, ".lever-files", true},
		{"github by name, external", []Tool{githubByName}, ".LEVER-FILES", true},
		{"whisper, unrelated dir", []Tool{whisperTool()}, "workers/w", false},
		{"whisper, a sibling name", []Tool{whisperTool()}, ".lever-files2", false},
		{"no such tool", []Tool{db}, ".lever-files", false},
		{"no tools", nil, ".lever-files", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t, tc.dir, "workers/other")
			a.Broker.Tools = tc.tools
			err := a.validateLeverFilesTools()
			if (err != nil) != tc.bad {
				t.Fatalf("err = %v, want refused=%v", err, tc.bad)
			}
			if err != nil && (!strings.Contains(err.Error(), `worker "alpha"`) || !strings.Contains(err.Error(), ".lever-files")) {
				t.Fatalf("the refusal must name the worker and .lever-files: %v", err)
			}
		})
	}
	// Through Validate too, with remote access off.
	a := testApp(t, ".lever-files", "workers/other")
	a.Broker.Tools = []Tool{{Name: "github", Command: []string{"/opt/lever/lever-tool-github", "-tree", a.Tree}, Backend: "127.0.0.1:3300", Operations: []Op{{Name: "push"}}}}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "overlaps .lever-files") {
		t.Fatalf("Validate = %v, want the .lever-files refusal", err)
	}
}
