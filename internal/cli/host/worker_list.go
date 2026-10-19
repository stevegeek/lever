package host

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/termsafe"
)

// workerListResult is `lever worker list --json`'s one line on stdout.
type workerListResult struct {
	OK      bool                     `json:"ok"`
	Workers []broker.WorkerListEntry `json:"workers,omitempty"`
	Reason  string                   `json:"reason,omitempty"`
	Error   string                   `json:"error,omitempty"`
	Exit    int                      `json:"exit"`
}

// maxListLabel bounds each string of a list entry. The broker sends known
// labels only; this holds the CLI to that even against a broker that does
// not.
const maxListLabel = 48

func boundedLabel(s string) string {
	s = termsafe.Sanitize(s)
	if len(s) > maxListLabel {
		s = strings.ToValidUTF8(s[:maxListLabel], "") + "…"
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

func sanitizeListEntry(e broker.WorkerListEntry) broker.WorkerListEntry {
	e.Name = boundedLabel(e.Name)
	e.Phase = boundedLabel(e.Phase)
	e.Activity = boundedLabel(e.Activity)
	e.ActivitySource = boundedLabel(e.ActivitySource)
	e.Container = boundedLabel(e.Container)
	return e
}

// newWorkerListCmd is `lever worker list`: each declared worker's record as
// the broker sees it. Read-only.
func newWorkerListCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List the declared workers: phase, activity (worker-reported), container, idle at prompt",
		Long: "List each declared worker as the broker sees it on the hub. The activity, and idle_at_prompt, which\n" +
			"derives from it, are reported by hooks inside the worker's own container: treat them as the worker's word.\n" +
			"--json prints one JSON object on stdout: ok, workers, reason, error, exit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fail := func(err error, reason string, code int) error {
				if asJSON {
					printJSONLine(cmd, workerListResult{Reason: reason, Error: err.Error(), Exit: code})
				}
				return cli.WithExitCode(fmt.Errorf("worker list: %w", err), code)
			}
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return fail(err, "", 1)
			}
			sock, err := operatorSocket(app, st)
			if err != nil {
				return fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
			}
			var resp broker.WorkerListResponse
			err = operatorGet(cmd.Context(), sock, broker.PathOperatorWorkerList, &resp)
			if ref, ok := operatorFailure(err, broker.PathOperatorWorkerList); ok {
				code := 1
				if ref.Reason == reasonNoOperatorSocket || ref.Reason == broker.ReasonRuntimeUnavailable {
					code = exitOperatorSocketMissing
				}
				return fail(ref, ref.Reason, code)
			} else if err != nil {
				return fail(err, "", 1)
			}
			workers := make([]broker.WorkerListEntry, 0, len(resp.Workers))
			for _, e := range resp.Workers {
				workers = append(workers, sanitizeListEntry(e))
			}
			if asJSON {
				printJSONLine(cmd, workerListResult{OK: true, Workers: workers})
				return nil
			}
			printWorkerTable(cmd, workers, time.Now())
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: ok, workers, reason, error, exit")
	return c
}

func printWorkerTable(cmd *cobra.Command, workers []broker.WorkerListEntry, now time.Time) {
	out := cmd.OutOrStdout()
	if len(workers) == 0 {
		fmt.Fprintln(out, "no workers are declared.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPHASE\tACTIVITY*\tLAST ACTIVITY\tCONTAINER\tIDLE AT PROMPT*")
	for _, e := range workers {
		last := "-"
		if e.LastActivity != nil {
			last = ago(now.Sub(*e.LastActivity))
		}
		idle := "unknown"
		if e.IdleAtPrompt != nil {
			idle = map[bool]string{true: "yes", false: "no"}[*e.IdleAtPrompt]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, e.Phase, dash(e.Activity), last, dash(e.Container), idle)
	}
	_ = tw.Flush()
	fmt.Fprintln(out, "* reported by the worker's own hooks: the worker's word, not lever's.")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ago renders an age for the table; --json carries the timestamp itself.
func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}
