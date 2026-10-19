package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/agentledger"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/state"
)

// checkSentLedger reports whether agents can verify lever's messages (package
// sentledger). Without it message_verify answers "unavailable" for every relay,
// note and directive notice, and the agents treat them as data.
func checkSentLedger(app *config.App, st state.State) checkResult {
	const name = "sent ledger"
	if inside, what, fix := stateInTree(app, st); inside {
		return checkResult{name, false, "off: " + what + " (agents mount the tree), so the broker keeps no record of what it sends and no agent can verify a lever message", fix}
	}
	p := st.SentLedger()
	running := brokerRunning(st)
	sock := ""
	if running {
		if fi, err := os.Stat(st.OperatorSock()); err != nil || fi.Mode()&fs.ModeSocket == 0 {
			return checkResult{name, false, "the broker is running but its operator socket " + stateRel(st, st.OperatorSock()) +
				" is absent, so `lever msg send` cannot send notes", "restart the broker (`lever apply`); a state path too long for a socket is reported in " + stateRel(st, st.OutLog())}
		}
		sock = "; operator socket present"
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return checkResult{name, true, "no sends recorded yet (" + stateRel(st, p) + "/)" + sock, ""}
	}
	if err != nil {
		return checkResult{name, false, "cannot read the sent ledger: " + err.Error(), "check " + stateRel(st, p)}
	}
	fix := "remove " + p + "; the broker writes a new one (messages sent before cannot be verified)"
	switch {
	case !fi.IsDir():
		return checkResult{name, false, "the sent ledger " + stateRel(st, p) + " is not a directory (a symlink?), so the broker sends nothing", fix}
	case fi.Mode().Perm()&0o022 != 0:
		return checkResult{name, false, fmt.Sprintf("the sent ledger %s is %v: another user can add a file, so the broker sends nothing", stateRel(st, p), fi.Mode().Perm()), "chmod 700 " + p}
	}
	if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
		return checkResult{name, false, fmt.Sprintf("the sent ledger %s belongs to uid %d, not to you (uid %d)", stateRel(st, p), owner, os.Getuid()), fix}
	}
	n, err := sentledger.Count(p)
	if err != nil {
		return checkResult{name, false, "the sent ledger is unsafe: " + err.Error(), fix}
	}
	return checkResult{name, true, fmt.Sprintf("%s/ (%d files, 0700)%s", stateRel(st, p), n, sock), ""}
}

func brokerRunning(st state.State) bool {
	_, found, alive := state.PIDStatus(st.PID())
	return found && alive
}

// guestClockWarn is the guest/host clock difference doctor starts to report.
const guestClockWarn = 2 * time.Second

// agentMessagesClockWarn is how far behind the host the guest may be, with
// agent messages on, before the row fails: half of agentledger.SkewBefore.
const agentMessagesClockWarn = 5 * time.Second

// checkGuestClock compares the guest's clock with the host's. A message is
// verified by its ref whatever the clocks say, so a drifted guest (Lima after
// the host slept) is only reported, except with agent messages on: the agent
// ledger binds a contact message only when its guest timestamp is at most
// agentledger.SkewBefore before its host-time authorization, so beyond that
// every authorized message is hidden from contacts.
func checkGuestClock(ctx context.Context, jr proc.Runner, now func() time.Time, agentMessages bool) checkResult {
	const name = "guest clock"
	before := now()
	res, err := jr.Run(proc.WithOutputLimit(ctx, smallGuestOutputLimit), nil, "date", "-u", "+%s")
	after := now()
	if err != nil {
		return checkResult{name, true, "not checked (the guest did not answer: " + firstLine(err.Error()) + ")", ""}
	}
	guest, perr := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if perr != nil {
		return checkResult{name, true, "not checked (unexpected `date` output)", ""}
	}
	mid := before.Add(after.Sub(before) / 2)
	diff := time.Unix(guest, 0).Sub(mid.Truncate(time.Second)) // < 0: the guest is behind
	skew := time.Duration(math.Abs(float64(diff)))
	// The guest reads whole seconds, so up to a second is resolution.
	detail := fmt.Sprintf("guest clock within %s of the host", skew.Round(time.Second))
	fix := "resync the guest clock (on Lima: `limactl shell <vm> sudo hwclock -s`, or restart the VM)"
	if agentMessages && diff < -agentMessagesClockWarn {
		return checkResult{name, false, fmt.Sprintf("guest clock is %s behind the host: with agent messages on, the hub stamps an agent's message before its authorization, and contacts are not shown it once that is more than %s",
			skew.Round(time.Second), agentledger.SkewBefore), fix}
	}
	if skew > guestClockWarn+time.Second {
		return warnResult(name, fmt.Sprintf("guest clock is %s off the host", skew.Round(time.Second)), fix)
	}
	return checkResult{name, true, detail, ""}
}
