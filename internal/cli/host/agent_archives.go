package host

import (
	"cmp"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/termsafe"
)

// `lever agent archives`, `restore` and `archive-delete`: the archives
// destroyed agents left (the broker's archives.go), a new agent restored
// from one, and an archive deleted for good.

func newAgentArchivesCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "archives",
		Args:  cobra.NoArgs,
		Short: "List the archives of destroyed agents",
		Long: "List every destroyed agent that left an archive: its workspace, its conversation transcript, or both.\n" +
			"An archive is restorable when it holds either and its template is still configured.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fail := func(err error, reason string, code int) error {
				if asJSON {
					printJSONLine(cmd, agentResult{Reason: reason, Error: err.Error(), Exit: code})
				}
				return cli.WithExitCode(fmt.Errorf("agent archives: %w", err), code)
			}
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return fail(err, "", 1)
			}
			sock, err := operatorSocket(app, st)
			if err != nil {
				return fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
			}
			var resp broker.ArchiveListResponse
			err = operatorGet(cmd.Context(), sock, broker.PathOperatorAgentArchives, &resp)
			if ref, ok := operatorFailure(err, broker.PathOperatorAgentArchives); ok {
				return fail(ref, ref.Reason, agentExit(ref.Reason))
			} else if err != nil {
				return fail(err, "", 1)
			}
			if asJSON {
				printJSONLine(cmd, resp)
				return nil
			}
			if len(resp.Archives) == 0 {
				cmd.Println("no archives of destroyed agents.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tTEMPLATE\tBY\tDESTROYED\tWORKSPACE\tTRANSCRIPT\tRESTORABLE\tLABEL")
			for _, a := range resp.Archives {
				restorable := "yes"
				switch {
				case a.Problem != "":
					restorable = "no: " + strings.ReplaceAll(termsafe.Sanitize(a.Problem), "\t", " ")
				case !a.TemplateConfigured:
					restorable = "with --template (its template is gone)"
				case !a.Restorable:
					restorable = "no"
				}
				destroyed := "-"
				if !a.DestroyedAt.IsZero() {
					destroyed = a.DestroyedAt.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", safeToken(a.Name), safeToken(a.Template), creatorOf(a.CreatedBy), destroyed,
					archiveSize(a.WorkspaceBytes), archiveSize(a.TranscriptBytes), restorable, strings.ReplaceAll(termsafe.Sanitize(a.Label), "\t", " "))
			}
			tw.Flush()
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the list as one JSON object")
	return c
}

// archiveSize is a compressed archive's size for the terminal.
func archiveSize(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1<<20:
		return fmt.Sprintf("%dKiB", max(n>>10, 1))
	}
	return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
}

func newAgentRestoreCmd() *cobra.Command {
	var template, label, memory, taskFile string
	var asJSON bool
	c := &cobra.Command{
		Use:   "restore ARCHIVE",
		Args:  cobra.ExactArgs(1),
		Short: "Spawn a new agent from a destroyed agent's archive",
		Long: "Spawn a new agent (a new name: names are never reused) from the archive of the destroyed agent ARCHIVE\n" +
			"(see `lever agent archives`). Its workspace is seeded from the archived workspace, and the old\n" +
			"conversation transcripts are written into it at " + broker.RestoredConversationDir + "/<ARCHIVE>/; its task tells it\n" +
			"to read the latest one. It runs under the same admission as any spawn (the template's caps and the memory\n" +
			"budget) and gets only its template's grants. --template defaults to the old agent's, --label to its label,\n" +
			"--memory to its size when the template still offers it. --task-file adds a task after lever's note.\n" +
			"Links, devices, unsafe names and archives past the limits are refused or skipped; see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res := agentResult{Template: template, Label: label, Memory: memory, RestoredFrom: args[0]}
			req := broker.RestoreRequest{Archive: args[0], Template: template, Label: label, Memory: memory}
			if taskFile != "" {
				app, _, err := loadAppAndState(nil)
				if err != nil {
					return cli.WithExitCode(fmt.Errorf("agent restore: %w", err), 1)
				}
				task, err := readTaskFile(app, taskFile)
				if err != nil {
					if asJSON {
						res.Reason, res.Error, res.Exit = reasonOf(err), err.Error(), exitWorkerRefused
						printJSONLine(cmd, res)
					}
					return cli.WithExitCode(fmt.Errorf("agent restore: %w", err), exitWorkerRefused)
				}
				req.Task = task
			}
			var resp broker.SpawnResponse
			if _, err := agentRun(cmd, "restore", broker.PathOperatorAgentRestore, req, &resp, &res, asJSON); err != nil {
				return err
			}
			res.OK, res.Name, res.Phase = true, safeToken(resp.Name), safeToken(resp.Phase)
			res.Template, res.Label = safeToken(resp.Template), termsafe.Sanitize(resp.Label)
			res.Memory = ""
			if _, ok := broker.ParseMemorySize(resp.Memory); ok {
				res.Memory = resp.Memory
			}
			if asJSON {
				printJSONLine(cmd, res)
				return nil
			}
			mem := ""
			if res.Memory != "" {
				mem = " with " + res.Memory + " of memory"
			}
			cmd.Printf("agent %s restored from %s into template %s%s; it is %s.\n", res.Name, safeToken(args[0]), res.Template, mem, cmp.Or(res.Phase, "in an unknown phase"))
			if r := resp.Restored; r != nil {
				cmd.Printf("seeded %d file(s)", r.Files)
				if r.Skipped > 0 {
					cmd.Printf(", skipped %d link(s), special file(s) or lever-owned path(s)", r.Skipped)
				}
				if r.Transcripts != "" {
					cmd.Printf("; transcripts in %s", termsafe.Sanitize(r.Transcripts))
				}
				cmd.Println(".")
			}
			return nil
		},
	}
	c.Flags().StringVar(&template, "template", "", "the template to spawn into (default: the old agent's)")
	c.Flags().StringVar(&label, "label", "", fmt.Sprintf("a title shown in lists (default: the old agent's; at most %d characters)", broker.MaxAgentLabel))
	c.Flags().StringVar(&memory, "memory", "", "the new agent's memory limit, one of the template's memory_sizes such as 8GiB")
	c.Flags().StringVar(&taskFile, "task-file", "", "a file holding a task added after lever's note")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: name, template, memory, restored_from, ok, phase, reason, error, exit")
	return c
}

func newAgentArchiveDeleteCmd() *cobra.Command {
	var force, asJSON bool
	c := &cobra.Command{
		Use:   "archive-delete NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a destroyed agent's archive for good",
		Long: "Delete the archive the destroyed agent NAME left (its workspace and conversation transcripts) and drop\n" +
			"its record, keeping only its name so it is never used again: what `lever agent gc` does by age.\n" +
			"Destructive, so it requires --force.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res := agentResult{Name: args[0]}
			if !force {
				err := fmt.Errorf("this deletes the archive of %s for good; re-run with --force to proceed", args[0])
				if asJSON {
					res.Reason, res.Error, res.Exit = reasonNoForce, err.Error(), exitWorkerRefused
					printJSONLine(cmd, res)
				}
				return cli.WithExitCode(fmt.Errorf("agent archive-delete: %w", err), exitWorkerRefused)
			}
			var resp broker.ArchiveDeleteResponse
			if _, err := agentRun(cmd, "archive-delete", broker.PathOperatorAgentArchiveDelete, broker.ArchiveDeleteRequest{Name: args[0]}, &resp, &res, asJSON); err != nil {
				return err
			}
			res.OK, res.Freed = true, resp.Freed
			if asJSON {
				printJSONLine(cmd, res)
				return nil
			}
			cmd.Printf("the archive of %s is deleted (%.1f MiB freed); its name stays retired.\n", safeToken(args[0]), float64(resp.Freed)/(1<<20))
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "confirm the deletion (required)")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: name, ok, freed_bytes, reason, error, exit")
	return c
}
