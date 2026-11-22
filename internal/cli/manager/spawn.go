package manager

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/wire"
)

func agentSpawn(c brokerCaller) *cobra.Command {
	var task, label string
	cmd := &cobra.Command{Use: "spawn TEMPLATE", Args: cobra.ExactArgs(1),
		Short: "Spawn a new agent from a template and start it with a task",
		Long: "Spawn a new agent from TEMPLATE and start it fresh with --task.\n\n" +
			"Only templates the operator lets the manager spawn from (`agent spawned` lists them). The broker\n" +
			"picks the agent's name (<prefix>-<6 characters>, never used before); its workspace, image, tools\n" +
			"and limits come from the template. --label is a title for lists only. Once it runs, drive it like a\n" +
			"worker (`agent suspend|resume|stop NAME`, `msg send --to NAME`), and destroy it when its work is\n" +
			"done. A template at its limit answers 409: destroy or suspend one of its agents first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := brokerCall[wire.AgentSpawnResponse](cmd.Context(), c, wire.PathAgentSpawn,
				wire.AgentSpawnRequest{Template: args[0], Task: task, Label: label})
			if err != nil {
				return err
			}
			cmd.Printf("%s: spawned from %s, %s\n", res.Name, res.Template, res.Phase)
			return nil
		}}
	cmd.Flags().StringVar(&task, "task", "Read your context, then begin.", "the new agent's task")
	cmd.Flags().StringVar(&label, "label", "", "a title shown in lists")
	return cmd
}

func agentDestroy(c brokerCaller) *cobra.Command {
	var stop bool
	cmd := &cobra.Command{Use: "destroy NAME", Args: cobra.ExactArgs(1),
		Short: "Destroy an agent you spawned: its conversation is backed up, then it is gone for good",
		Long: "Destroy an agent the manager spawned. Its conversation is backed up on the host first, its name is\n" +
			"revoked for good, its workspace is handled as its template says (archived by default) and its record\n" +
			"is deleted. A running agent is refused (409) unless --stop. Agents the operator spawned and\n" +
			"configured workers are never yours to destroy.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := brokerCall[wire.AgentDestroyResponse](cmd.Context(), c, wire.PathAgentDestroy,
				wire.AgentDestroyRequest{Name: args[0], Stop: stop})
			if err != nil {
				return err
			}
			cmd.Printf("%s: destroyed (was %s); workspace %s\n", res.Name, res.Was, res.Workspace)
			for _, w := range res.Warnings {
				cmd.Printf("  warning: %s\n", w)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&stop, "stop", false, "stop the agent first if it is running")
	return cmd
}

func agentSpawned(c brokerCaller) *cobra.Command {
	return &cobra.Command{Use: "spawned", Args: cobra.NoArgs,
		Short: "List the templates you may spawn from and the agents you spawned",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := brokerCall[wire.AgentListResponse](cmd.Context(), c, wire.PathAgentList, struct{}{})
			if err != nil {
				return err
			}
			if len(res.Templates) == 0 {
				cmd.Println("No template lets the manager spawn agents.")
			}
			for _, t := range res.Templates {
				cmd.Printf("template %s: %d/%d agents, %d/%d running\n", t.Name, t.Instances, t.MaxInstances, t.Running, t.MaxRunning)
			}
			for _, a := range res.Agents {
				line := fmt.Sprintf("  %s  (%s)  %s", a.Name, a.Template, a.State)
				if a.Phase != "" {
					line += " [" + a.Phase + "]"
				}
				if a.Label != "" {
					line += "  — " + strings.ReplaceAll(a.Label, "\n", " ")
				}
				cmd.Println(line)
			}
			return nil
		}}
}
