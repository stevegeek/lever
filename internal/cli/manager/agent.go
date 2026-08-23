package manager

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/wire"
)

// workerCall is brokerCall specialized to the worker-command response shape.
func workerCall(ctx context.Context, c brokerCaller, endpoint string, body any) (workerResult, error) {
	return brokerCall[workerResult](ctx, c, endpoint, body)
}

func newAgentCmd(c brokerCaller) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Drive worker agents via the broker"}
	cmd.AddCommand(agentList(c), agentStart(c), agentStop(c), agentSuspend(c), agentResume(c))
	return cmd
}

func agentStart(c brokerCaller) *cobra.Command {
	var task string
	cmd := &cobra.Command{Use: "start NAME", Args: cobra.ExactArgs(1),
		Short: "Start a worker agent (fresh); to resume an existing one use `agent resume`",
		Long: "Start a worker agent with a task.\n\n" +
			"To bring an EXISTING (suspended/stopped) worker back up, use `lever-manager agent resume NAME` —\n" +
			"a worker's task is fixed at creation, so `agent start` against an existing worker with a\n" +
			"(new) task returns HTTP 409. Run `lever worker purge NAME` first to discard the old record and\n" +
			"start it fresh with a new task. (Because --task defaults to a non-empty prompt, `agent start`\n" +
			"never carries an empty task, so it cannot itself resume — that is what `agent resume` is for.)",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := workerCall(cmd.Context(), c, wire.PathWorkerStart,
				wire.WorkerStartRequest{Worker: args[0], Task: task})
			if err != nil {
				return err
			}
			cmd.Printf("%s: %s\n", res.Worker, res.Phase)
			return nil
		}}
	cmd.Flags().StringVar(&task, "task", "Read your context, then begin.", "task/boot prompt")
	return cmd
}

func agentVerb(c brokerCaller, use, short, endpoint string) *cobra.Command {
	return &cobra.Command{Use: use + " NAME", Args: cobra.ExactArgs(1), Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := workerCall(cmd.Context(), c, endpoint, wire.WorkerRequest{Worker: args[0]})
			if err != nil {
				return err
			}
			cmd.Printf("%s: %s\n", res.Worker, res.Phase)
			return nil
		}}
}

func agentStop(c brokerCaller) *cobra.Command {
	return agentVerb(c, "stop", "Stop a worker agent", wire.PathWorkerStop)
}
func agentSuspend(c brokerCaller) *cobra.Command {
	return agentVerb(c, "suspend", "Suspend a worker agent", wire.PathWorkerSuspend)
}
func agentResume(c brokerCaller) *cobra.Command {
	return agentVerb(c, "resume", "Resume a worker agent", wire.PathWorkerResume)
}

func agentList(c brokerCaller) *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List worker agents", RunE: func(cmd *cobra.Command, _ []string) error {
		res, err := workerCall(cmd.Context(), c, wire.PathWorkerList, struct{}{})
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
