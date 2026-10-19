package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/wire"
)

// Exit codes of `lever worker resume` (and exitWorkerNotRunning, of
// `lever msg send`). A script branches on these, or on the reason in --json,
// never on the text. 3 and 4 mean what they mean for the manager in `lever
// up`. They are documented in docs-site/_reference/cli.md.
const (
	exitWorkerResumeFailed    = cli.ExitManagerResumeFailed // 3: did not resume or come live; the record is kept
	exitWorkerHubRefused      = cli.ExitHubRefusedResume    // 4: the hub refused the resume; the record is kept
	exitWorkerNoRecord        = 8                           // the hub has no record: the manager starts the worker
	exitWorkerNotRunning      = 9                           // msg send: the worker is not running; nothing was sent
	exitWorkerRefused         = 10                          // refused for good: config, identity or workspace; fix the cause
	exitOperatorSocketMissing = 11                          // no broker answers on the operator socket
	exitWorkerBusy            = 75                          // another start/resume/heal is under way, or too many workers run: try again later
)

// workerResumeExit maps a broker refusal reason to the command's exit code.
func workerResumeExit(reason string) int {
	switch reason {
	case broker.ReasonBusy, broker.ReasonWorkerCap:
		return exitWorkerBusy
	case broker.ReasonNoRecord:
		return exitWorkerNoRecord
	case broker.ReasonHubRefused:
		return exitWorkerHubRefused
	case broker.ReasonNotLive, broker.ReasonRuntimeError:
		return exitWorkerResumeFailed
	case broker.ReasonRuntimeUnavailable:
		return exitOperatorSocketMissing
	case broker.ReasonNotAWorker, broker.ReasonRevoked, broker.ReasonRoleMismatch,
		broker.ReasonWorkspaceUnsafe, broker.ReasonWorkspaceMissing, broker.ReasonSharedAccess:
		return exitWorkerRefused
	}
	return 1
}

// workerResumeResult is --json's one line on stdout, for success and failure.
type workerResumeResult struct {
	Worker string `json:"worker"`
	OK     bool   `json:"ok"`
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Exit   int    `json:"exit"`
}

// newWorkerResumeCmd is `lever worker resume NAME`: the manager's resume verb
// run by the host operator through the broker's 0600 operator socket. It is
// the documented replacement for host programs that called the remote page's
// /operator/wake route directly.
func newWorkerResumeCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "resume NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Resume a declared worker that is not running, and wait until it is live",
		Long: "Resume a declared worker that is not running (suspended, stopped, completed, or error) through the broker,\n" +
			"with the checks and the per-worker lock of the manager's `agent resume`, and wait until it is live.\n" +
			"A running worker is left alone (success). Exit codes and --json reasons: see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			res := workerResumeResult{Worker: name}
			fail := func(err error, reason string, code int) error {
				if asJSON {
					res.Reason, res.Error, res.Exit = reason, err.Error(), code
					printJSONLine(cmd, res)
				}
				return cli.WithExitCode(fmt.Errorf("worker resume %s: %w", name, err), code)
			}
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return fail(err, "", 1)
			}
			if !declaredWorker(app.Workers, name) {
				return fail(fmt.Errorf("unknown worker %q (declared: %s); the manager is resumed with `lever up`", name, strings.Join(workerNamesOf(app), ", ")),
					broker.ReasonNotAWorker, exitWorkerRefused)
			}
			if brokerctl.StateInsideTree(app, st) {
				return fail(fmt.Errorf("the state directory %s is inside the tree, so the broker serves no operator socket; move the instance's state out of the tree", st.Dir),
					"no-operator-socket", exitOperatorSocketMissing)
			}
			sock := st.OperatorSock()
			if _, err := os.Stat(sock); errors.Is(err, fs.ErrNotExist) {
				return fail(fmt.Errorf("%w; run `lever up`", errNoOperatorSocket), "no-operator-socket", exitOperatorSocketMissing)
			}
			var resp wire.WorkerResponse
			err = operatorPost(cmd.Context(), sock, broker.PathOperatorWorkerResume, wire.WorkerRequest{Worker: name}, &resp)
			var ref *operatorRefusal
			var uerr *url.Error
			switch {
			case errors.As(err, &ref) && ref.Status == http.StatusNotFound && ref.Reason == "":
				// No reason and no route: a broker older than this lever.
				return fail(fmt.Errorf("the running broker has no %s route (it is older than this lever); run `lever reload`", broker.PathOperatorWorkerResume),
					"no-operator-socket", exitOperatorSocketMissing)
			case errors.As(err, &ref):
				res.Phase = ref.Phase
				return fail(ref, ref.Reason, workerResumeExit(ref.Reason))
			case errors.As(err, &uerr):
				return fail(fmt.Errorf("the broker did not answer on its operator socket (%v); it may be restarting under `lever up` or `lever reload`: try again, or run `lever up`", err), "no-operator-socket", exitOperatorSocketMissing)
			case err != nil:
				return fail(err, "", 1)
			}
			res.OK, res.Phase = true, safeToken(resp.Phase)
			if asJSON {
				printJSONLine(cmd, res)
				return nil
			}
			cmd.Printf("worker %s is %s.\n", name, res.Phase)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: worker, ok, phase, reason, error, exit")
	return c
}

// printJSONLine writes v as one line on stdout (cobra's Print* go to stderr).
func printJSONLine(cmd *cobra.Command, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(cmd.OutOrStdout(), string(b))
}

func declaredWorker(ws []config.Worker, name string) bool {
	for _, w := range ws {
		if w.Name == name {
			return true
		}
	}
	return false
}
