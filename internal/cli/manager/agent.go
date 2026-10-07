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
	cmd := &cobra.Command{Use: "agent", Short: "Drive worker agents via the broker",
		Long: "Drive worker agents via the broker (manager only).\n\n" +
			"NAME is a worker's bare name as the instance config declares it: no agent: or user: prefix\n" +
			"(those are `msg send --to` address forms; see `lever-manager msg send --help`)."}
	cmd.AddCommand(agentList(c), agentStart(c), agentStop(c), agentSuspend(c), agentResume(c), agentRecycle(c))
	return cmd
}

func agentStart(c brokerCaller) *cobra.Command {
	var task string
	cmd := &cobra.Command{Use: "start NAME", Args: cobra.ExactArgs(1),
		Short: "Start a worker agent (fresh); to resume an existing one use `agent resume`",
		Long: "Start a worker agent with a task.\n\n" +
			"To bring an EXISTING (suspended/stopped) worker back up, use `lever-manager agent resume NAME` —\n" +
			"a worker's task is fixed at creation, so `agent start` against an existing worker with a\n" +
			"(new) task returns HTTP 409. To discard the old record and start it fresh with a new task, use\n" +
			"`lever-manager agent recycle NAME --task` for a worker the config marks recyclable, else ask the\n" +
			"operator to run `lever worker purge NAME`. (Because --task defaults to a non-empty prompt, `agent start`\n" +
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

func agentRecycle(c brokerCaller) *cobra.Command {
	var task string
	cmd := &cobra.Command{Use: "recycle NAME", Args: cobra.ExactArgs(1),
		Short: "Discard a stopped worker's record and start it fresh with a new task",
		Long: "Discard a worker's record and start it fresh with a new task, in one call.\n\n" +
			"For a worker slot reused for new work that needs a FRESH context. The worker's old\n" +
			"conversation is lost; its workspace (work product) is kept. Only a worker the config marks\n" +
			"`recyclable: true` (403 otherwise), and only one that is suspended, stopped or in phase error\n" +
			"(409 otherwise: run `agent stop NAME` first). At most one recycle of a worker per minute (429).\n" +
			"To continue the old conversation instead, use `agent resume NAME`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := workerCall(cmd.Context(), c, wire.PathWorkerRecycle,
				wire.WorkerStartRequest{Worker: args[0], Task: task})
			if err != nil {
				return err
			}
			cmd.Printf("%s: %s\n", res.Worker, res.Phase)
			return nil
		}}
	cmd.Flags().StringVar(&task, "task", "Read your context, then begin.", "task/boot prompt of the fresh worker")
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
