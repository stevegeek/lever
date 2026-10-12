package config

import (
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/voice"
)

// githubProgram is the base name of lever-tool-github's program.
const githubProgram = "lever-tool-github"

// isGithubCommand reports whether t is the github tool: named github, its
// program (after an env(1) prefix) lever-tool-github, or a command that
// takes -app-key (the GitHub App key flag, no other lever tool has it).
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

// validateLeverFilesTools refuses a worker dir that overlaps the tree
// root's .lever-files (case-folded, like the remote.files check) while a
// tool reads the manager's files there: the whisper tool transcribes WAVs
// from .lever-files/whisper for any caller, and the github tool pushes
// bundles from .lever-files/github. A worker mounts its dir read-write, so
// one over or inside .lever-files could plant what those tools then act on
// as the manager's. remote.files makes the same refusal (validateRemote),
// which runs only with remote access on; this one does not depend on it.
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
