package config

import (
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/voice"
)

const githubProgram = "lever-tool-github"

// isGithubCommand also matches a command that takes -app-key, which no other
// lever tool has.
func isGithubCommand(t Tool) bool {
	if t.Name == "github" {
		return true
	}
	if t.External || len(t.Command) == 0 {
		return false
	}
	argv, _ := unwrapEnv(t.Command)
	if len(argv) == 0 {
		return false
	}
	if baseName(argv[0]) == githubProgram {
		return true
	}
	for _, a := range argv[1:] {
		name := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if a != name && (name == "app-key" || strings.HasPrefix(name, "app-key=")) {
			return true
		}
	}
	return false
}

// validateLeverFilesTools refuses a worker dir that overlaps .lever-files
// while a tool acts on the manager's files there (whisper transcribes clips,
// github pushes bundles): a worker mounts its dir read-write, so it could
// plant what those tools act on as the manager's. Unlike the remote.files
// check, this does not depend on remote access.
func (a *App) validateLeverFilesTools() error {
	var reason string
	for _, t := range a.Broker.Tools {
		switch {
		case isWhisperCommand(t):
			reason = fmt.Sprintf("broker.tools[%s] (whisper) transcribes the manager's clips from %s", t.Name, voice.AgentDir)
		case isGithubCommand(t):
			reason = fmt.Sprintf("broker.tools[%s] (github) pushes the manager's bundles from %s/github", t.Name, chatfiles.Dir)
		default:
			continue
		}
		break
	}
	if reason == "" {
		return nil
	}
	for _, w := range a.Workers {
		if fsutil.RelOverlapFold(w.Dir, chatfiles.Dir) {
			return fmt.Errorf("config: worker %q dir %q overlaps %s, and %s: the worker could plant files there; move the worker", w.Name, w.Dir, chatfiles.Dir, reason)
		}
	}
	return nil
}
