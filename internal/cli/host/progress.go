package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
	"golang.org/x/sys/unix"
)

// progressFlagUsage documents --progress on apply and up.
const progressFlagUsage = "progress output: auto (a live line on a terminal, else plain), tty, plain, json or quiet; default from LEVER_PROGRESS"

// bringup is one `lever apply` / `lever up` run's progress list and record.
type bringup struct {
	rep   *progress.Reporter
	state state.State
}

// startBringup builds the run's progress list (lever#185): the renderer
// from --progress, else LEVER_PROGRESS, else the terminal; a record of the
// run in the state directory; command tracing when LEVER_TRACE is set
// (lever#184). It prints the header: what runs, on which backend — the first
// line of every bring-up, before the jail is touched (lever#195).
func startBringup(cmd *cobra.Command, app *config.App, st state.State, command, flag string) (*bringup, context.Context, error) {
	mode, set, err := progress.ParseMode(flag)
	if err != nil {
		return nil, nil, err
	}
	if !set {
		if mode, set, err = progress.ParseMode(os.Getenv("LEVER_PROGRESS")); err != nil {
			return nil, nil, fmt.Errorf("LEVER_PROGRESS: %w", err)
		}
	}
	out := cmd.ErrOrStderr()
	if !set {
		mode = progress.AutoMode(out, isTerminal, os.Getenv)
	}
	opts := progress.Options{Mode: mode, Out: out, Command: command, Trace: traceOn(), Width: termWidth(out)}
	if st.Dir != "" {
		if path, err := progress.RecordPath(st.BringupLogDir(), command, time.Now()); err == nil {
			opts.Record = path
		}
	}
	rep := progress.New(opts)
	rep.Header(bringupHeader(command, app))
	ctx := progress.With(cmd.Context(), rep)
	if opts.Trace {
		ctx = proc.WithTracer(ctx, rep)
	}
	return &bringup{rep: rep, state: st}, ctx, nil
}

// bringupHeader is the first line: the command, the application and the
// backend's containment posture.
func bringupHeader(command string, app *config.App) string {
	h := fmt.Sprintf("lever %s: application %q", command, app.Name)
	if p, ok := registry.ProfileFor(app.Backend); ok {
		h += " — backend: " + p.Summary()
	}
	return h
}

// finish ends the run and saves its summary for doctor. Call it before an
// attach: the terminal must be the manager's from then on.
func (b *bringup) finish(err error) {
	if b == nil {
		return
	}
	sum := b.rep.Finish(err)
	if b.state.Dir != "" && sum.Outcome != "" {
		_ = progress.WriteLast(b.state.LastBringup(), sum)
	}
}

// traceOn reads LEVER_TRACE: any value but "", "0" and "false".
func traceOn() bool {
	switch strings.ToLower(os.Getenv("LEVER_TRACE")) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	return err == nil
}

// termWidth is w's width in columns, or 0 when unknown.
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}

// tailLines is how many lines a failed step's tail shows.
const tailLines = 15

// tailTimeout bounds reading a tail from the guest.
const tailTimeout = 10 * time.Second

// stepTail names, per apply step, the log that explains its failure
// (lever#186): the host broker and proxy logs, and for the steps that talk
// to the hub, the hub's own log in the guest.
//
// The manager container's log is deliberately not shown: it is the
// agent's word (jail.AgentProbe.LogTail), matched for fixed phrases but
// never printed.
func (w *applyWiring) stepTail(kind apply.StepKind) func() (string, []string) {
	switch kind {
	case apply.KindBrokerUp:
		p := w.state.OutLog()
		return func() (string, []string) { return p, fileTail(p, tailLines) }
	case apply.KindRemoteProxy:
		p := w.state.RemoteLog()
		return func() (string, []string) { return p, fileTail(p, tailLines) }
	case apply.KindScionServer, apply.KindRegisterProject, apply.KindAgentTemplate,
		apply.KindCredential, apply.KindStartManager:
		return func() (string, []string) {
			return "the hub's log in the jail (~/" + layout.ServerLogRel + ")", w.guestTail(layout.ServerLogRel)
		}
	}
	return nil
}

// guestTail reads the last lines of a file under the jail user's home,
// bounded in time and size. rel is a fixed lever path, never input.
func (w *applyWiring) guestTail(rel string) []string {
	if w.jr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), tailTimeout)
	defer cancel()
	ctx = proc.WithOutputLimit(ctx, 64<<10)
	res, err := w.jr.Run(ctx, nil, "sh", "-c", fmt.Sprintf(`tail -n %d "$HOME/%s" 2>/dev/null`, tailLines, rel))
	if err != nil && res.Stdout == "" {
		return nil
	}
	return redactLines(strings.Split(res.Stdout, "\n"))
}

// fileTail returns up to n last lines of a host file (nil when unreadable),
// with credentials masked.
func fileTail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const max = 64 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		f.Seek(-max, io.SeekEnd)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return redactLines(lines)
}

func redactLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, scion.RedactSecrets(l))
	}
	return out
}

// checkLastBringup is doctor's row for the last `lever apply` / `lever up`:
// how long it took and its slowest steps (lever#185), read from the summary
// the run saved. It is information, never a failure: a failed run already
// said why on the terminal, and its record is in the logs directory.
func checkLastBringup(st state.State, now time.Time) checkResult {
	const name = "last bring-up"
	sum, err := progress.ReadLast(st.LastBringup())
	switch {
	case errors.Is(err, progress.ErrNoRecord):
		return checkResult{name, true, "none recorded yet (the next `lever apply` or `lever up` records one)", ""}
	case err != nil:
		return checkResult{name, true, "summary unreadable: " + err.Error(), ""}
	}
	return checkResult{name, true, sum.Describe(now) + "; records in " + st.BringupLogDir(), ""}
}

// daemonLogging makes a foreground daemon (`lever broker serve`, `lever
// remote serve`) write lever's daemon log lines (internal/logging): its
// slog and standard-log output, and its cobra prints, which take the
// operator prose form and are logged at that form's level.
func daemonLogging(cmd *cobra.Command, component string) {
	l := logging.Install(cmd.ErrOrStderr(), component)
	w := logging.Writer(l)
	cmd.SetOut(w)
	cmd.SetErr(w)
}
