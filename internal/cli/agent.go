package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// workerCallFn is the active broker caller (seam for tests).
var workerCallFn = workerCall

// managerBootstrapPath is where the manager's own bootstrap.json is readable
// from inside the manager CONTAINER, where scion mounts the tree at /workspace
// (the jail-level /lever mount does not exist in the container), so the
// bootstrap deposited by `lever apply` at <tree>/.lever/bootstrap.json appears
// here. Tests can override it.
var managerBootstrapPath = "/workspace/.lever/bootstrap.json"

// managerIDDir is the directory holding the manager's mTLS identity (cert+key+ca).
// It is the "~/.lever-id" path resolved at process start. Tests can override it.
var managerIDDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lever-id")
}()

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Drive grove agents via the broker"}
	cmd.AddCommand(agentList(), agentStart(), agentStop(), agentSuspend(), agentResume())
	return cmd
}

func agentStart() *cobra.Command {
	var task string
	c := &cobra.Command{Use: "start NAME", Args: cobra.ExactArgs(1), Short: "Start (or resume) a grove agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := workerCallFn(cmd.Context(), "/grove/start",
				map[string]string{"grove": args[0], "task": task})
			if err != nil {
				return err
			}
			cmd.Printf("%s: %s\n", res.Worker, res.Phase)
			return nil
		}}
	c.Flags().StringVar(&task, "task", "Read your context, then begin.", "task/boot prompt")
	return c
}

func agentVerb(use, short, endpoint string) *cobra.Command {
	return &cobra.Command{Use: use + " NAME", Args: cobra.ExactArgs(1), Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := workerCallFn(cmd.Context(), endpoint, map[string]string{"grove": args[0]})
			if err != nil {
				return err
			}
			cmd.Printf("%s: %s\n", res.Worker, res.Phase)
			return nil
		}}
}

func agentStop() *cobra.Command { return agentVerb("stop", "Stop a grove agent", "/grove/stop") }
func agentSuspend() *cobra.Command {
	return agentVerb("suspend", "Suspend a grove agent", "/grove/suspend")
}
func agentResume() *cobra.Command {
	return agentVerb("resume", "Resume a grove agent", "/grove/resume")
}

func agentList() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List grove agents", RunE: func(cmd *cobra.Command, _ []string) error {
		res, err := workerCallFn(cmd.Context(), "/grove/list", map[string]string{})
		if err != nil {
			return err
		}
		if len(res.Agents) == 0 {
			cmd.Println("No running agents.")
			return nil
		}
		for _, a := range res.Agents {
			line := "  " + a.Slug + "  [" + a.Phase + "]"
			if a.Activity != "" {
				line += "  — " + a.Activity
			}
			cmd.Println(line)
		}
		return nil
	}}
}
