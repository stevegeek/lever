package host

import (
	"cmp"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/termsafe"
)

// newAgentCmd is `lever agent`: agents spawned from the templates in
// lever.yaml. Every verb goes through the broker's operator socket.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Spawn and destroy agents from templates"}
	cmd.AddCommand(newAgentSpawnCmd(), newAgentDestroyCmd(), newAgentListCmd(), newAgentGCCmd(), newAgentRemoteVerbCmd(),
		newAgentArchivesCmd(), newAgentRestoreCmd(), newAgentArchiveDeleteCmd())
	return cmd
}

// agentResult is the --json line of spawn and destroy.
type agentResult struct {
	Name       string   `json:"name,omitempty"`
	Template   string   `json:"template,omitempty"`
	Label      string   `json:"label,omitempty"`
	Memory     string   `json:"memory,omitempty"`
	OK         bool     `json:"ok"`
	Phase      string   `json:"phase,omitempty"`
	Was        string   `json:"was,omitempty"`
	Transcript string   `json:"transcript,omitempty"`
	Workspace  string   `json:"workspace,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	// RestoredFrom (restore) names the archive; Freed (archive-delete) is
	// the bytes deleted.
	RestoredFrom string `json:"restored_from,omitempty"`
	Freed        int64  `json:"freed_bytes,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
	Exit         int    `json:"exit"`
}

// agentExit maps a broker refusal to an exit code: the worker verbs' codes,
// plus spawn's and destroy's own refusals.
func agentExit(reason string) int {
	switch reason {
	case broker.ReasonTemplateFull, broker.ReasonTemplateBusy, broker.ReasonMemoryBusy:
		return exitWorkerBusy
	case broker.ReasonRunning:
		return exitWorkerNotStopped
	case broker.ReasonNoTemplate, broker.ReasonBadLabel, broker.ReasonBadMemory, broker.ReasonNotSpawned, broker.ReasonNotYours, broker.ReasonBadRequest,
		broker.ReasonNoArchive, broker.ReasonArchiveRefused:
		return exitWorkerRefused
	case broker.ReasonBackupFailed:
		return exitWorkerResumeFailed
	}
	return workerVerbExit(reason)
}

// agentRun posts one agent verb and fills res; it prints res as JSON on
// failure when asJSON.
func agentRun(cmd *cobra.Command, verb, path string, body, out any, res *agentResult, asJSON bool) (*config.App, error) {
	fail := func(err error, reason string, code int) error {
		if asJSON {
			res.Reason, res.Error, res.Exit = reason, err.Error(), code
			printJSONLine(cmd, res)
		}
		return cli.WithExitCode(fmt.Errorf("agent %s: %w", verb, err), code)
	}
	app, st, err := loadAppAndState(nil)
	if err != nil {
		return nil, fail(err, "", 1)
	}
	sock, err := operatorSocket(app, st)
	if err != nil {
		return nil, fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
	}
	err = operatorPost(cmd.Context(), sock, path, body, out)
	if ref, ok := operatorFailure(err, path); ok {
		res.Phase = ref.Phase
		return nil, fail(ref, ref.Reason, agentExit(ref.Reason))
	} else if err != nil {
		return nil, fail(err, "", 1)
	}
	return app, nil
}

func newAgentSpawnCmd() *cobra.Command {
	var taskFile, label, memory string
	var asJSON bool
	c := &cobra.Command{
		Use:   "spawn TEMPLATE",
		Args:  cobra.ExactArgs(1),
		Short: "Spawn a new agent from a template and start it",
		Long: "Spawn a new agent from TEMPLATE (under `templates:` in lever.yaml) and start it fresh with the task\n" +
			"in --task-file. The broker picks its name (<prefix>-<6 characters>, never used before), creates its\n" +
			"workspace under the template's dir_root and grants it the template's capabilities. The label is shown\n" +
			"in lists only. --memory picks one of the template's memory_sizes (default: its resources.memory); the\n" +
			"spawn is refused when the memory budget has too little free.\nExit codes and --json reasons: see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res := agentResult{Template: args[0], Label: label, Memory: memory}
			req := broker.SpawnRequest{Template: args[0], Label: label, Memory: memory}
			if taskFile != "" {
				app, _, err := loadAppAndState(nil)
				if err != nil {
					return cli.WithExitCode(fmt.Errorf("agent spawn: %w", err), 1)
				}
				task, err := readTaskFile(app, taskFile)
				if err != nil {
					if asJSON {
						res.Reason, res.Error, res.Exit = reasonOf(err), err.Error(), exitWorkerRefused
						printJSONLine(cmd, res)
					}
					return cli.WithExitCode(fmt.Errorf("agent spawn: %w", err), exitWorkerRefused)
				}
				req.Task = task
			}
			var resp broker.SpawnResponse
			if _, err := agentRun(cmd, "spawn", broker.PathOperatorAgentSpawn, req, &resp, &res, asJSON); err != nil {
				return err
			}
			res.OK, res.Name, res.Phase = true, safeToken(resp.Name), safeToken(resp.Phase)
			if resp.Memory != "" {
				res.Memory = memoryToken(resp.Memory)
			}
			if asJSON {
				printJSONLine(cmd, res)
				return nil
			}
			mem := ""
			if res.Memory != "" {
				mem = " with " + res.Memory + " of memory"
			}
			cmd.Printf("agent %s spawned from template %s%s; it is %s.\n", res.Name, args[0], mem, cmp.Or(res.Phase, "in an unknown phase"))
			return nil
		},
	}
	c.Flags().StringVar(&taskFile, "task-file", "", "a file holding the new agent's task")
	c.Flags().StringVar(&label, "label", "", fmt.Sprintf("a title shown in lists (at most %d characters)", broker.MaxAgentLabel))
	c.Flags().StringVar(&memory, "memory", "", "the new agent's memory limit, one of the template's memory_sizes such as 8GiB (default: its resources.memory)")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: name, template, memory, ok, phase, reason, error, exit")
	return c
}

func newAgentDestroyCmd() *cobra.Command {
	var stop, noBackup, force, asJSON bool
	c := &cobra.Command{
		Use:   "destroy NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Destroy a spawned agent: back up its conversation, revoke it, handle its workspace, delete it",
		Long: "Destroy the spawned agent NAME. Its conversation is backed up to the state directory first (a failed\n" +
			"backup stops the destroy unless --no-backup). Then its name is revoked for good, its workspace is\n" +
			"archived, removed or kept as its template's on_destroy says, and its hub record is deleted. A running\n" +
			"agent is refused unless --stop. Destructive, so it requires --force.\n" +
			"Exit codes and --json reasons: see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res := agentResult{Name: args[0]}
			if !force {
				err := fmt.Errorf("this destroys agent %s for good; re-run with --force to proceed", args[0])
				if asJSON {
					res.Reason, res.Error, res.Exit = reasonNoForce, err.Error(), exitWorkerRefused
					printJSONLine(cmd, res)
				}
				return cli.WithExitCode(fmt.Errorf("agent destroy: %w", err), exitWorkerRefused)
			}
			var resp broker.DestroyResponse
			if _, err := agentRun(cmd, "destroy", broker.PathOperatorAgentDestroy, broker.DestroyRequest{Name: args[0], Stop: stop, NoBackup: noBackup}, &resp, &res, asJSON); err != nil {
				return err
			}
			res.OK, res.Was = true, cmp.Or(safeToken(resp.Was), "in an unknown phase")
			if convbackup.ValidID(resp.Transcript) {
				res.Transcript = resp.Transcript
			}
			res.Workspace = termsafe.Sanitize(resp.Workspace)
			for _, w := range resp.Warnings {
				res.Warnings = append(res.Warnings, termsafe.Sanitize(w))
			}
			if asJSON {
				printJSONLine(cmd, res)
				return nil
			}
			for _, w := range res.Warnings {
				cmd.PrintErrf("lever: warning: %s\n", w)
			}
			backup := "no conversation backup"
			if res.Transcript != "" {
				backup = "conversation backed up as " + res.Transcript
			}
			cmd.Printf("agent %s destroyed (it was %s): %s; workspace %s; its name is revoked for good.\n", res.Name, res.Was, backup, res.Workspace)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "confirm the destroy (required)")
	c.Flags().BoolVar(&stop, "stop", false, "stop the agent first if it is running")
	c.Flags().BoolVar(&noBackup, "no-backup", false, "destroy without backing up its conversation")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: name, ok, was, transcript, workspace, warnings, reason, error, exit")
	return c
}

func newAgentListCmd() *cobra.Command {
	var all, asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List the templates and the agents spawned from them",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fail := func(err error, reason string, code int) error {
				if asJSON {
					printJSONLine(cmd, agentResult{Reason: reason, Error: err.Error(), Exit: code})
				}
				return cli.WithExitCode(fmt.Errorf("agent list: %w", err), code)
			}
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return fail(err, "", 1)
			}
			sock, err := operatorSocket(app, st)
			if err != nil {
				return fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
			}
			path := broker.PathOperatorAgentList
			if all {
				path += "?all=1"
			}
			var resp broker.AgentListResponse
			err = operatorGet(cmd.Context(), sock, path, &resp)
			if ref, ok := operatorFailure(err, broker.PathOperatorAgentList); ok {
				return fail(ref, ref.Reason, agentExit(ref.Reason))
			} else if err != nil {
				return fail(err, "", 1)
			}
			if asJSON {
				printJSONLine(cmd, resp)
				return nil
			}
			printAgentList(cmd, resp)
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include destroyed agents")
	c.Flags().BoolVar(&asJSON, "json", false, "print the list as one JSON object")
	return c
}

func printAgentList(cmd *cobra.Command, resp broker.AgentListResponse) {
	if len(resp.Templates) == 0 && len(resp.Agents) == 0 {
		cmd.Println("no templates and no spawned agents.")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TEMPLATE\tPREFIX\tAGENTS\tRUNNING\tMEMORY\tSPAWNERS")
	for _, t := range resp.Templates {
		spawners := "operator"
		if t.ManagerMay {
			spawners += ", manager"
		}
		var sizes []string
		for _, sz := range t.MemorySizes {
			n := memoryToken(sz.Name)
			if sz.Name == t.MemoryDefault {
				n += "*"
			}
			sizes = append(sizes, n)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%d/%d\t%s\t%s\n", safeToken(t.Name), safeToken(t.Prefix), t.Instances, t.MaxInstances, t.Running, t.MaxRunning,
			cmp.Or(strings.Join(sizes, " "), "-"), spawners)
	}
	tw.Flush()
	if m := resp.Memory; m != nil {
		cmd.Printf("memory budget: %s used of %s, %s free (running and starting agents' limits; * is a template's default size)\n",
			fmtBytes(m.UsedBytes), fmtBytes(m.BudgetBytes), fmtBytes(m.FreeBytes))
	}
	if len(resp.Agents) == 0 {
		return
	}
	cmd.Println()
	tw = tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tTEMPLATE\tSTATE\tPHASE\tMEMORY\tBY\tCREATED\tLABEL")
	for _, a := range resp.Agents {
		state := safeToken(a.State)
		if a.Orphaned {
			state += " (template gone or moved)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", safeToken(a.Name), safeToken(a.Template), state, cmp.Or(safeToken(a.Phase), "-"),
			cmp.Or(memoryToken(a.Memory), "-"), creatorOf(a.CreatedBy), a.CreatedAt.Format("2006-01-02 15:04"), strings.ReplaceAll(termsafe.Sanitize(a.Label), "\t", " "))
	}
	tw.Flush()
	var queued []string
	for _, a := range resp.Agents {
		if a.QueuedMessages > 0 {
			queued = append(queued, fmt.Sprintf("%s %d", safeToken(a.Name), a.QueuedMessages))
		}
	}
	printQueuedLine(cmd.OutOrStdout(), queued)
}

// creatorOf is a record's creator for the terminal: an agent or manager
// name, "operator", or a remote caller ("ssh:<name>").
func creatorOf(s string) string {
	if broker.ValidRemoteCaller(s) {
		return s
	}
	return safeToken(s)
}

func newAgentGCCmd() *cobra.Command {
	var older string
	var force, dryRun, asJSON bool
	c := &cobra.Command{
		Use:   "gc",
		Args:  cobra.NoArgs,
		Short: "Delete the archives and conversation backups of agents destroyed long ago",
		Long: "For every agent destroyed at least --older ago (default 30d), delete its archive (workspace and\n" +
			"conversation, or the older separate workspace archive and conversation backups) from the state\n" +
			"directory, and drop its record, keeping only its name so it is never used again. A workspace kept in\n" +
			"the tree (on_destroy: keep) is not touched. Destructive, so it requires --force; --dry-run lists what\n" +
			"would go. `lever agent archive-delete NAME` deletes one archive now.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fail := func(err error, reason string, code int) error {
				if asJSON {
					printJSONLine(cmd, agentResult{Reason: reason, Error: err.Error(), Exit: code})
				}
				return cli.WithExitCode(fmt.Errorf("agent gc: %w", err), code)
			}
			age, err := config.ParseDays(older)
			if err != nil || age < 0 {
				return fail(fmt.Errorf("--older: %v", err), broker.ReasonBadRequest, exitWorkerRefused)
			}
			if !force && !dryRun {
				return fail(errors.New("this deletes archives and conversation backups for good; re-run with --force, or with --dry-run to see what would go"), reasonNoForce, exitWorkerRefused)
			}
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return fail(err, "", 1)
			}
			sock, err := operatorSocket(app, st)
			if err != nil {
				return fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
			}
			var resp broker.GCResponse
			err = operatorPost(cmd.Context(), sock, broker.PathOperatorAgentGC, broker.GCRequest{OlderThan: int64(age / time.Second), DryRun: dryRun}, &resp)
			if ref, ok := operatorFailure(err, broker.PathOperatorAgentGC); ok {
				return fail(ref, ref.Reason, agentExit(ref.Reason))
			} else if err != nil {
				return fail(err, "", 1)
			}
			names := make([]string, 0, len(resp.Agents))
			for _, n := range resp.Agents {
				names = append(names, safeToken(n))
			}
			warnings := make([]string, 0, len(resp.Warnings))
			for _, w := range resp.Warnings {
				warnings = append(warnings, termsafe.Sanitize(w))
			}
			if asJSON {
				printJSONLine(cmd, broker.GCResponse{Agents: names, Freed: resp.Freed, DryRun: resp.DryRun, Warnings: warnings})
				return nil
			}
			for _, w := range warnings {
				cmd.PrintErrf("lever: warning: %s\n", w)
			}
			verb := "collected"
			if dryRun {
				verb = "would collect"
			}
			cmd.Printf("%s %d destroyed agent(s), %.1f MiB: %s\n", verb, len(names), float64(resp.Freed)/(1<<20), cmp.Or(strings.Join(names, ", "), "none"))
			return nil
		},
	}
	c.Flags().StringVar(&older, "older", "30d", "only agents destroyed at least this long ago (e.g. 90d, 12h, 0)")
	c.Flags().BoolVar(&force, "force", false, "confirm the deletion (required unless --dry-run)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted, and delete nothing")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object: agents, freed_bytes, dry_run, warnings")
	return c
}

// fmtBytes spells n bytes as GiB when whole, else MiB.
func fmtBytes(n int64) string {
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%dGiB", n>>30)
	}
	return fmt.Sprintf("%dMiB", n>>20)
}

// memoryToken is s when it is a memory size in the fixed format ("8GiB"),
// else "": safeToken would drop it for its capitals.
func memoryToken(s string) string {
	if _, ok := broker.ParseMemorySize(s); ok {
		return s
	}
	return ""
}
