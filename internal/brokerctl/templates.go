package brokerctl

import (
	"context"
	"errors"
	"fmt"
	"github.com/stevegeek/lever/internal/fsutil"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
			SkillRel:    agentSkillRel,
			Skill:       skill,
			IdleSuspend: t.IdleSuspend,
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
		return saveAgentConversation(ctx, app, st, jr, jailMount, version, agent, "destroy")
	}
}

// backupMinFree is the space a conversation backup leaves free.
const backupMinFree = 2 << 30

// dailyBackupDue is how old an agent's newest backup must be before the
// daily backup takes another. A little under a day, so an hourly check does
// not drift a day later each time.
const dailyBackupDue = 23 * time.Hour

// agentDailyBackup is the broker's DailyBackup: it saves agent's
// conversation when its newest backup is dailyBackupDue old or more, then
// keeps the newest convbackup.Keep.
func agentDailyBackup(app *config.App, st state.State, jr proc.Runner, jailMount, version string) func(ctx context.Context, agent string) (string, error) {
	return func(ctx context.Context, agent string) (string, error) {
		store := convbackup.Store{Dir: filepath.Join(st.AgentConversations(), agent)}
		list, err := store.List()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		for _, m := range list {
			if time.Since(m.Created) < dailyBackupDue {
				return "", nil
			}
		}
		id, err := saveAgentConversation(ctx, app, st, jr, jailMount, version, agent, "daily")
		if err != nil || id == "" {
			return id, err
		}
		if _, err := store.Prune(convbackup.Keep, id); err != nil {
			return id, fmt.Errorf("pruning old backups: %w", err)
		}
		return id, nil
	}
}

// gcNameRE is a spawned agent's name: gc joins it to a host path.
var gcNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// removeAgentConversations is the broker's RemoveConversations: it deletes
// agent's backup directory under the state directory (no link followed: the
// directory and every entry are Lstat'ed) and returns the bytes it held.
func removeAgentConversations(app *config.App, st state.State) func(agent string, dryRun bool) (int64, error) {
	return func(agent string, dryRun bool) (int64, error) {
		if !gcNameRE.MatchString(agent) {
			return 0, fmt.Errorf("%q is not an agent name", agent)
		}
		if StateInsideTree(app, st) {
			return 0, nil // no backup is kept there
		}
		dir := filepath.Join(st.AgentConversations(), agent)
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if !fi.IsDir() {
			return 0, fmt.Errorf("%s is not a directory; left for the operator", dir)
		}
		var size int64
		err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				if i, err := d.Info(); err == nil {
					size += i.Size()
				}
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		if dryRun {
			return size, nil
		}
		return size, os.RemoveAll(dir)
	}
}

// saveAgentConversation captures agent's conversation from its home in the
// guest and saves it under the state directory with reason. "" with no
// error: there was nothing to save.
func saveAgentConversation(ctx context.Context, app *config.App, st state.State, jr proc.Runner, jailMount, version, agent, reason string) (string, error) {
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
	// A backup never fills the host's disk: it needs its own size and
	// backupMinFree besides.
	if free, err := fsutil.FreeBytes(st.AgentConversations()); err != nil {
		return "", fmt.Errorf("reading the free space for the backup: %w", err)
	} else if free < backupMinFree+uint64(len(raw)) {
		return "", fmt.Errorf("less than %d GiB would be left free in the state directory after the backup", backupMinFree>>30)
	}
	store := convbackup.Store{Dir: filepath.Join(st.AgentConversations(), agent)}
	m, err := store.Save(convbackup.Meta{Agent: agent, Created: time.Now().UTC(), Reason: reason, Version: version},
		func(w io.Writer) (convbackup.Stats, error) {
			return convbackup.Copy(w, strings.NewReader(raw), "projects", convbackup.DefaultLimits)
		})
	if err != nil {
		return "", fmt.Errorf("saving the conversation: %w", err)
	}
	return m.ID, nil
}

// checkRegistryNames refuses a config that gives a fixed agent the name of a
// spawned agent in the registry: that agent's certificates would
// authenticate as it. A template's prefix that changed can do it.
func checkRegistryNames(app *config.App, st broker.AgentsState) error {
	fixed := map[string]bool{app.Name: true, app.ManagerCN(): true}
	for _, w := range app.Workers {
		fixed[w.Name] = true
	}
	for _, r := range st.Agents {
		if fixed[r.Name] {
			return fmt.Errorf("brokerctl: %q is both a configured agent and a spawned agent in %s (state %s); rename the configured one: a spawned agent's name is never reused", r.Name, "agents.json", r.State)
		}
	}
	for _, n := range st.Retired {
		if fixed[n] {
			return fmt.Errorf("brokerctl: %q is both a configured agent and a destroyed agent's retired name in %s; rename the configured one: a spawned agent's name is never reused", n, "agents.json")
		}
	}
	return nil
}
