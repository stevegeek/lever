package host

import (
	"cmp"
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
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

// Exit codes of the `lever worker` verbs that go through the broker (and
// exitWorkerNotRunning, of `lever msg send`). A script branches on these, or
// on the reason in --json, never on the text. 3 and 4 mean what they mean
// for the manager in `lever up`. They are documented in
// docs-site/_reference/cli.md.
const (
	exitWorkerResumeFailed    = cli.ExitManagerResumeFailed // 3: did not start, resume or come live; the record is kept
	exitWorkerHubRefused      = cli.ExitHubRefusedResume    // 4: the hub refused the resume; the record is kept
	exitWorkerNoRecord        = 8                           // the hub has no record
	exitWorkerNotRunning      = 9                           // msg send: the worker is not running; nothing was sent
	exitWorkerRefused         = 10                          // refused for good: config, identity, task or workspace; fix the cause
	exitOperatorSocketMissing = 11                          // no broker answers on the operator socket
	exitWorkerNotStopped      = 12                          // recycle: the worker is live; stop or suspend it first
	exitWorkerBusy            = 75                          // another start/resume/heal is under way, too many workers run, or a recycle is rate limited: try again later
)

// The CLI's own reasons, for what it refuses before the broker answers.
const (
	reasonNoOperatorSocket = "no-operator-socket"
	reasonTaskFile         = "task-file"
)

// workerVerbExit maps a broker refusal reason to the command's exit code.
func workerVerbExit(reason string) int {
	switch reason {
	case broker.ReasonBusy, broker.ReasonWorkerCap, broker.ReasonRateLimited:
		return exitWorkerBusy
	case broker.ReasonNoRecord:
		return exitWorkerNoRecord
	case broker.ReasonHubRefused:
		return exitWorkerHubRefused
	case broker.ReasonNotLive, broker.ReasonRuntimeError:
		return exitWorkerResumeFailed
	case broker.ReasonRuntimeUnavailable, reasonNoOperatorSocket:
		return exitOperatorSocketMissing
	case broker.ReasonNotStopped:
		return exitWorkerNotStopped
	case broker.ReasonNotAWorker, broker.ReasonRevoked, broker.ReasonRoleMismatch,
		broker.ReasonWorkspaceUnsafe, broker.ReasonWorkspaceMissing, broker.ReasonSharedAccess,
		broker.ReasonBadTask, broker.ReasonTaskTooLong, broker.ReasonTaskFixed,
		broker.ReasonConfigFile, reasonTaskFile:
		return exitWorkerRefused
	}
	return 1
}

// workerVerbResult is --json's one line on stdout, for success and failure.
type workerVerbResult struct {
	Worker string `json:"worker"`
	OK     bool   `json:"ok"`
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Exit   int    `json:"exit"`
}

// workerVerb is one `lever worker` verb that the broker runs: its route, and
// the request body once the worker is known to be declared.
type workerVerb struct {
	name string // "resume", "start", ...
	path string
	body func(app *config.App, worker string) (any, error)
}

// runWorkerVerb resolves the instance, checks name is a declared worker and
// that a broker serves the operator socket, then asks it to run v.
func runWorkerVerb(cmd *cobra.Command, v workerVerb, name string, asJSON bool) error {
	res := workerVerbResult{Worker: name}
	fail := func(err error, reason string, code int) error {
		if asJSON {
			res.Reason, res.Error, res.Exit = reason, err.Error(), code
			printJSONLine(cmd, res)
		}
		return cli.WithExitCode(fmt.Errorf("worker %s %s: %w", v.name, name, err), code)
	}
	app, st, err := loadAppAndState(nil)
	if err != nil {
		return fail(err, "", 1)
	}
	if !declaredWorker(app.Workers, name) {
		return fail(fmt.Errorf("unknown worker %q (declared: %s); the manager is run with `lever up`", name, strings.Join(workerNamesOf(app), ", ")),
			broker.ReasonNotAWorker, exitWorkerRefused)
	}
	sock, err := operatorSocket(app, st)
	if err != nil {
		return fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
	}
	body, err := v.body(app, name)
	if err != nil {
		return fail(err, reasonOf(err), workerVerbExit(reasonOf(err)))
	}
	var resp wire.WorkerResponse
	err = operatorPost(cmd.Context(), sock, v.path, body, &resp)
	if ref, ok := operatorFailure(err, v.path); ok {
		res.Phase = ref.Phase
		return fail(ref, ref.Reason, workerVerbExit(ref.Reason))
	} else if err != nil {
		return fail(err, "", 1)
	}
	res.OK, res.Phase = true, safeToken(resp.Phase)
	if asJSON {
		printJSONLine(cmd, res)
		return nil
	}
	cmd.Printf("worker %s is %s.\n", name, cmp.Or(res.Phase, "in an unknown phase"))
	return nil
}

// reasonError is a local refusal with its --json reason.
type reasonError struct {
	reason string
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }
func (e *reasonError) Unwrap() error { return e.err }

func reasonOf(err error) string {
	var re *reasonError
	if errors.As(err, &re) {
		return re.reason
	}
	return ""
}

// operatorSocket is the broker's operator socket, once it is known to exist.
func operatorSocket(app *config.App, st state.State) (string, error) {
	if brokerctl.StateInsideTree(app, st) {
		return "", fmt.Errorf("the state directory %s is inside the tree, so the broker serves no operator socket; move the instance's state out of the tree", st.Dir)
	}
	sock := st.OperatorSock()
	if _, err := os.Stat(sock); errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w; run `lever up`", errNoOperatorSocket)
	}
	return sock, nil
}

// operatorFailure splits an operator-socket error: a refusal with its reason
// comes back as ok, a missing route or a socket nobody answers on as an
// error carrying the reason no-operator-socket.
func operatorFailure(err error, path string) (*operatorRefusal, bool) {
	var ref *operatorRefusal
	var uerr *url.Error
	switch {
	case errors.As(err, &ref) && ref.Status == http.StatusNotFound && ref.Reason == "":
		// No reason and no route: a broker older than this lever.
		return &operatorRefusal{Status: ref.Status, Reason: reasonNoOperatorSocket,
			Text: fmt.Sprintf("the running broker has no %s route (it is older than this lever); run `lever reload`", path)}, true
	case errors.As(err, &ref):
		return ref, true
	case errors.As(err, &uerr):
		return &operatorRefusal{Reason: reasonNoOperatorSocket,
			Text: fmt.Sprintf("the broker did not answer on its operator socket (%v); it may be restarting under `lever up` or `lever reload`: try again, or run `lever up`", err)}, true
	}
	return nil, false
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
			return runWorkerVerb(cmd, workerVerb{name: "resume", path: broker.PathOperatorWorkerResume, body: plainWorkerBody}, args[0], asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, jsonFlagUsage)
	return c
}

const jsonFlagUsage = "print one JSON object on stdout: worker, ok, phase, reason, error, exit"

func plainWorkerBody(_ *config.App, worker string) (any, error) {
	return wire.WorkerRequest{Worker: worker}, nil
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
