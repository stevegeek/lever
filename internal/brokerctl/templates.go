package brokerctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/skills"
	"github.com/stevegeek/lever/internal/state"
)

// agentSkillRel is a worker's lever skill, relative to its workspace
// (sessionrec.SkillRel).
const agentSkillRel = ".claude/skills/lever-agent/SKILL.md"

// workerSpecOf is one worker's spec. A spawned agent is a worker built from
// its template (config.Template.Worker) with no shared folders: shared
// folders name their readers and writers, and an instance is none of them.
func workerSpecOf(app *config.App, jailMount, jailUID string, g config.Worker, shared bool) broker.WorkerSpec {
	ticketDir := ""
	if jailUID != "" {
		ticketDir = jail.WorkerTicketDir(jailUID, g.Name)
	}
	spec := broker.WorkerSpec{
		Name:            g.Name,
		WorkspaceSubdir: g.Dir,                          // relative to the project root (/lever); scion mounts this subtree at /workspace
		HostWorkspace:   filepath.Join(app.Tree, g.Dir), // <tree>/<dir>, made before start (scion's guard requires it to exist)
		TicketDir:       ticketDir,
		Image:           app.WorkerAgentImage(g),
		Model:           app.WorkerModel(g), // own model:, else the manager's; empty ⇒ scion decides
		// Own instructions_file only, never the manager's. Read at dispatch.
		InstructionsPath: app.WorkerInstructionsPath(g),
		TaskPath:         app.WorkerTaskPath(g),
		APIKey:           app.EffectiveWorkerLLMAuth(g) == config.LLMAuthAPIKey,
		Recyclable:       g.Recyclable,
		Claude:           g.Claude(),
		// Own resources only — never the manager's.
		Resources: apply.ScionResources(g.Resources),
	}
	if shared {
		spec.Shared = sharedMounts(app, jailMount, g.Name)
	}
	return spec
}

// TemplateSpecs is every template as the broker needs it. version stamps
// the lever skill written into each new workspace.
func TemplateSpecs(app *config.App, jailMount, jailUID, version string) []broker.TemplateSpec {
	skill := skills.AgentWith(version, ChatConfigured(app), app.AgentMessagesOn(),
		skills.Files{On: app.FilesOn(), NoUploads: app.FilesOn() && !app.FilesUploadsOn(), NoShares: app.FilesOn() && !app.FilesSharesOn()})
	out := make([]broker.TemplateSpec, 0, len(app.Templates))
	for _, t := range app.Templates {
		t := t
		ts := broker.TemplateSpec{
			Name: t.Name, Prefix: t.EffectivePrefix(), DirRoot: filepath.ToSlash(filepath.Clean(t.DirRoot)),
			MaxInstances: t.MaxInstances, MaxRunning: t.MaxRunning,
			OnDestroy:  t.EffectiveOnDestroy(),
			ManagerMay: t.MaySpawn(config.SpawnerManager),
			Worker: func(name string) broker.WorkerSpec {
				return workerSpecOf(app, jailMount, jailUID, t.Worker(name), false)
			},
			SkillRel: agentSkillRel,
			Skill:    skill,
		}
		for _, g := range t.Obtain {
			ts.Obtain = append(ts.Obtain, rules.Grant{Tool: g.Tool, Op: g.Op})
		}
		for _, d := range t.Delegate {
			ts.Delegate = append(ts.Delegate, rules.Delegation{Tool: d.Tool, Op: d.Op, To: append([]string(nil), d.To...)})
		}
		out = append(out, ts)
	}
	return out
}

// errBackupInTree: lever keeps no conversation where an agent can read it.
var errBackupInTree = errors.New("the state directory is inside the tree the agents mount, so lever keeps no conversation backup there")

// agentConversationBackup saves agent's conversation from its home in the
// jail to <state>/agent-conversations/<agent>/. An agent with no home or no
// transcripts has nothing to save ("", nil).
func agentConversationBackup(app *config.App, st state.State, jr proc.Runner, jailMount, version string) func(ctx context.Context, agent string) (string, error) {
	return func(ctx context.Context, agent string) (string, error) {
		if StateInsideTree(app, st) {
			return "", errBackupInTree
		}
		raw, found, _, err := convbackup.Guest{R: jr, Workspace: jailMount, Agent: agent}.Capture(ctx)
		if errors.Is(err, convbackup.ErrNoHome) || (err == nil && !found) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(st.AgentConversations(), 0o700); err != nil {
			return "", err
		}
		store := convbackup.Store{Dir: filepath.Join(st.AgentConversations(), agent)}
		m, err := store.Save(convbackup.Meta{Agent: agent, Created: time.Now().UTC(), Reason: "destroy", Version: version},
			func(w io.Writer) (convbackup.Stats, error) {
				return convbackup.Copy(w, strings.NewReader(raw), "projects", convbackup.DefaultLimits)
			})
		if err != nil {
			return "", fmt.Errorf("saving the conversation: %w", err)
		}
		return m.ID, nil
	}
}
