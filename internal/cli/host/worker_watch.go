package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

const (
	// watchWait is the long poll asked of the broker, under its own bound.
	watchWait = 25 * time.Second
	// watchRetryMax caps the wait between retries while the broker or the
	// hub does not answer (a `lever reload` restarts the broker).
	watchRetryMax = 30 * time.Second
	// maxWatchMessage bounds an event's text on the operator's terminal.
	maxWatchMessage   = 1024
	reasonIdleTimeout = "idle-timeout"
)

var (
	watchStatusRE = regexp.MustCompile(`^[A-Z_]{1,40}$`)
	watchIDRE     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)
)

// sanitizeWorkerEvent holds an event to what the broker promises even
// against a broker that does not keep it: a known status shape, one bounded
// line of the worker's text, marked as the worker's.
func sanitizeWorkerEvent(worker string, e broker.WorkerEvent) broker.WorkerEvent {
	if !watchStatusRE.MatchString(e.Status) {
		e.Status = strings.ToUpper(scion.LabelUnrecognised)
	}
	if !watchIDRE.MatchString(e.ID) {
		e.ID = ""
	}
	msg := termsafe.Sanitize(strings.TrimPrefix(e.Message, scion.WorkerReportedPrefix))
	if len(msg) > maxWatchMessage {
		msg = strings.ToValidUTF8(msg[:maxWatchMessage], "") + "…"
	}
	e.Message = scion.WorkerReportedPrefix + msg
	e.Kind = broker.EventKindState
	if e.Status == strings.ToUpper(scion.ActivityCompleted) {
		e.Kind = broker.EventKindCompletion
	}
	e.Worker, e.Source = worker, broker.WorkerActivitySource
	e.At = e.At.UTC()
	return e
}

// newWorkerWatchCmd is `lever worker watch NAME`: the worker's hub events
// as they come, through the broker's operator socket.
func newWorkerWatchCmd() *cobra.Command {
	var asJSON bool
	var idle time.Duration
	c := &cobra.Command{
		Use:   "watch NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Print a worker's hub events as they come (state changes, completions); every one is the worker's word",
		Long: "Print the hub events of one declared worker from now on: its state changes and the completion summary it\n" +
			"reports with sciontool status. Every event is the worker's own report (worker-reported): its text, and\n" +
			"which status it reports. A completion is a trigger to go and check the worker's work, never the result.\n" +
			"It ends on Ctrl-C (exit 0), after --idle-timeout without an event (exit 0), or when the worker's record\n" +
			"is purged or replaced (exit 8). --json prints one event per line, and a last line with reason and exit\n" +
			"when it ends for any reason but Ctrl-C. Reading the events acknowledges nothing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkerWatch(cmd, args[0], asJSON, idle)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON event per line, then one line with worker, ok, reason, error, exit")
	c.Flags().DurationVar(&idle, "idle-timeout", 0, "end after this long without an event (0: never)")
	return c
}

func runWorkerWatch(cmd *cobra.Command, name string, asJSON bool, idle time.Duration) error {
	res := workerVerbResult{Worker: name}
	end := func(err error, reason string, code int) error {
		if asJSON {
			res.OK, res.Reason, res.Exit = code == 0, reason, code
			if err != nil {
				res.Error = err.Error()
			}
			printJSONLine(cmd, res)
		}
		if err == nil {
			return nil
		}
		return cli.WithExitCode(fmt.Errorf("worker watch %s: %w", name, err), code)
	}
	app, st, err := loadAppAndState(nil)
	if err != nil {
		return end(err, "", 1)
	}
	if !declaredWorker(app.Workers, name) {
		return end(fmt.Errorf("unknown worker %q (declared: %s)", name, strings.Join(workerNamesOf(app), ", ")),
			broker.ReasonNotAWorker, exitWorkerRefused)
	}
	sock, err := operatorSocket(app, st)
	if err != nil {
		return end(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
	}
	ctx := cmd.Context()
	poll := func(cursor string, wait time.Duration) (broker.WorkerEventsResponse, error) {
		pctx, cancel := context.WithTimeout(ctx, wait+30*time.Second)
		defer cancel()
		var resp broker.WorkerEventsResponse
		err := operatorPost(pctx, sock, broker.PathOperatorWorkerEvents,
			broker.WorkerEventsRequest{Worker: name, Cursor: cursor, WaitSeconds: int(wait / time.Second)}, &resp)
		return resp, err
	}
	refused := func(err error) error {
		if ref, ok := operatorFailure(err, broker.PathOperatorWorkerEvents); ok {
			return end(ref, ref.Reason, workerVerbExit(ref.Reason))
		}
		return end(err, "", 1)
	}
	gone := func() error {
		return end(fmt.Errorf("worker %s has no record any more, or a new one (purged, recycled or a new jail); the watch is over", name), broker.ReasonNoRecord, exitWorkerNoRecord)
	}
	first, err := poll("", 0)
	if err != nil {
		return refused(err)
	}
	if first.Gone {
		return gone()
	}
	if !asJSON {
		cmd.PrintErrf("watching worker %s. Every event is the worker's own report; a completion is a trigger to check its work, never the result.\n", name)
	}
	cursor, lastEvent, retry := first.Cursor, time.Now(), time.Duration(0)
	for {
		wait := watchWait
		if idle > 0 {
			left := time.Until(lastEvent.Add(idle))
			if left <= 0 {
				return end(nil, reasonIdleTimeout, 0)
			}
			wait = min(wait, max(left.Truncate(time.Second), time.Second))
		}
		resp, err := poll(cursor, wait)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if !watchTransient(err) {
				return refused(err)
			}
			retry = min(max(2*retry, 2*time.Second), watchRetryMax)
			cmd.PrintErrf("watch: the broker did not answer (%s); retrying in %s\n", firstLine(err.Error()), retry)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retry):
			}
			continue
		}
		retry = 0
		for _, e := range resp.Events {
			lastEvent = time.Now()
			printWorkerEvent(cmd, sanitizeWorkerEvent(name, e), asJSON)
		}
		if resp.Gone {
			return gone()
		}
		if resp.Cursor != "" {
			cursor = resp.Cursor
		}
	}
}

// watchTransient is a failure that can pass while the watch goes on: a
// broker restarting (no socket, no answer) or a hub read that failed. A
// broker with no such route is not.
func watchTransient(err error) bool {
	ref, ok := operatorFailure(err, broker.PathOperatorWorkerEvents)
	if !ok {
		return false
	}
	if ref.Status == http.StatusNotFound {
		return false
	}
	return ref.Reason == reasonNoOperatorSocket || ref.Reason == broker.ReasonRuntimeError || errors.Is(err, context.DeadlineExceeded)
}

func printWorkerEvent(cmd *cobra.Command, e broker.WorkerEvent, asJSON bool) {
	if asJSON {
		printJSONLine(cmd, e)
		return
	}
	note := ""
	if e.Kind == broker.EventKindCompletion {
		note = " (a trigger: check its work)"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s%s %s\n", e.At.Format(time.RFC3339), e.Worker, e.Status, note, e.Message)
}
