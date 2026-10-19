package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// maxTaskFileBytes bounds what is read of --task-file. scion.CheckTask then
// holds the task to scion's much smaller command-line budget.
const maxTaskFileBytes = 64 << 10

// readTaskFile reads --task-file on the host. The operator may name a file in
// the tree, which agents write, so a symbolic link there is refused: it
// could point the read at any file the operator can read, and the task is
// handed to a worker. It must be a regular file (a FIFO would hang the
// read), non-empty, and a task scion can start.
func readTaskFile(app *config.App, path string) (string, error) {
	raw, err := fsutil.ReadFileNoTreeLinks(path, app.Tree, maxTaskFileBytes)
	if err != nil {
		return "", &reasonError{reasonTaskFile, fmt.Errorf("--task-file %s: %w", path, err)}
	}
	task := strings.TrimSpace(string(raw))
	if task == "" {
		return "", &reasonError{reasonTaskFile, fmt.Errorf("--task-file %s is empty; omit the flag to use the worker's task_file", path)}
	}
	if err := scion.CheckTask(task); err != nil {
		reason := broker.ReasonBadTask
		var tooLong *scion.TaskTooLongError
		if errors.As(err, &tooLong) {
			reason = broker.ReasonTaskTooLong
		}
		return "", &reasonError{reason, fmt.Errorf("--task-file %s: %w", path, err)}
	}
	return task, nil
}

// taskBody is the body of start and recycle: the --task-file's task, or none,
// which leaves the broker to the worker's task_file.
func taskBody(taskFile *string) func(*config.App, string) (any, error) {
	return func(app *config.App, worker string) (any, error) {
		req := wire.WorkerStartRequest{Worker: worker}
		if *taskFile != "" {
			task, err := readTaskFile(app, *taskFile)
			if err != nil {
				return nil, err
			}
			req.Task = task
		}
		return req, nil
	}
}

// newWorkerStartCmd is `lever worker start NAME`: the manager's `agent start`
// run by the operator.
func newWorkerStartCmd() *cobra.Command {
	var asJSON bool
	var taskFile string
	c := &cobra.Command{
		Use:   "start NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Start a declared worker (or resume its record), and wait until it is live",
		Long: "Start a declared worker through the broker, as the manager's `agent start` does, and wait until it is live.\n" +
			"A worker with no record is created with the task from --task-file, else the worker's task_file.\n" +
			"An existing record is resumed (its task is fixed at creation, so --task-file is refused then);\n" +
			"a running worker is left alone. Exit codes and --json reasons: see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkerVerb(cmd, workerVerb{name: "start", path: broker.PathOperatorWorkerStart, body: taskBody(&taskFile)}, args[0], asJSON)
		},
	}
	c.Flags().StringVar(&taskFile, "task-file", "", "a file holding the worker's task (default: the worker's task_file)")
	c.Flags().BoolVar(&asJSON, "json", false, jsonFlagUsage)
	return c
}

// newWorkerRecycleCmd is `lever worker recycle NAME`: the manager's
// `agent recycle` run by the operator.
func newWorkerRecycleCmd() *cobra.Command {
	var asJSON bool
	var taskFile string
	c := &cobra.Command{
		Use:   "recycle NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Discard a recyclable worker's record and start it fresh (keeps its workspace)",
		Long: "Discard a worker's record and start it fresh through the broker, as the manager's `agent recycle` does:\n" +
			"only for a worker with recyclable: true, only when it is suspended, stopped or in phase error, and at most\n" +
			"once a minute per worker (the manager's recycles count too). Its old conversation is lost; its workspace\n" +
			"is kept. The task is --task-file's, else the worker's task_file. Exit codes and --json reasons: see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkerVerb(cmd, workerVerb{name: "recycle", path: broker.PathOperatorWorkerRecycle, body: taskBody(&taskFile)}, args[0], asJSON)
		},
	}
	c.Flags().StringVar(&taskFile, "task-file", "", "a file holding the worker's new task (default: the worker's task_file)")
	c.Flags().BoolVar(&asJSON, "json", false, jsonFlagUsage)
	return c
}

func newWorkerHaltCmd(verb, path, short string) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   verb + " NAME",
		Args:  cobra.ExactArgs(1),
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkerVerb(cmd, workerVerb{name: verb, path: path, body: plainWorkerBody}, args[0], asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, jsonFlagUsage)
	return c
}

func newWorkerSuspendCmd() *cobra.Command {
	return newWorkerHaltCmd("suspend", broker.PathOperatorWorkerSuspend,
		"Suspend a declared worker through the broker (its conversation is kept; `lever worker resume` brings it back)")
}

func newWorkerStopCmd() *cobra.Command {
	return newWorkerHaltCmd("stop", broker.PathOperatorWorkerStop,
		"Stop a declared worker through the broker (a contact's wake can't undo it; `lever worker resume` can)")
}

// autostartWorker is the converge step of a worker with autostart: the
// operator's start verb with no task, so the broker resumes a record, leaves
// a running worker alone, or starts a new one with the worker's task_file.
func autostartWorker(ctx context.Context, app *config.App, st state.State, worker string) error {
	sock, err := operatorSocket(app, st)
	if err != nil {
		return err
	}
	var resp wire.WorkerResponse
	err = operatorPost(ctx, sock, broker.PathOperatorWorkerStart, wire.WorkerStartRequest{Worker: worker}, &resp)
	if ref, ok := operatorFailure(err, broker.PathOperatorWorkerStart); ok {
		if ref.Reason != "" {
			return fmt.Errorf("%w (reason %s)", ref, ref.Reason)
		}
		return ref
	}
	return err
}
