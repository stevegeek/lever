package host

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
)

// guestBootProbe is what doctor reads about how the previous guest boot
// ended.
type guestBootProbe struct {
	sudo  bool      // sudo -n worked: the journal was readable
	last  time.Time // the previous boot's last journal entry; zero when there is no previous boot
	clean bool      // PID 1 logged reaching shutdown in the previous boot
}

// guestBootProbeScript reads the previous boot from the persistent journal.
// Only PID 1's messages decide "clean": _PID is set by journald, and a
// container's log lines, which the agent writes, cannot claim it.
const guestBootProbeScript = `J='sudo -n journalctl -q --no-pager -b -1'
if sudo -n true 2>/dev/null; then echo sudo=1; else echo sudo=0; exit 0; fi
echo "last=$($J -n 1 -o short-unix 2>/dev/null | head -n 1 | cut -d ' ' -f 1)"
if $J _PID=1 -n 30 -o cat 2>/dev/null | grep -qE 'Reached target.*(shutdown\.target|[Ss]hutdown)|^Shutting down'; then echo clean=1; else echo clean=0; fi
`

func parseGuestBootProbe(out string) guestBootProbe {
	var p guestBootProbe
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "sudo":
			p.sudo = v == "1"
		case "clean":
			p.clean = v == "1"
		case "last":
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1e11 {
				p.last = time.Unix(int64(f), 0).UTC()
			}
		}
	}
	return p
}

func readGuestBootProbe(ctx context.Context, jr proc.Runner) (guestBootProbe, error) {
	res, err := jr.Run(ctx, nil, "sh", "-c", guestBootProbeScript)
	if err != nil {
		return guestBootProbe{}, err
	}
	return parseGuestBootProbe(res.Stdout), nil
}

const guestBootFixPanic = "read the saved console logs: a kernel panic reboots the guest after 10 s, so its trace is before the last boot banner; while the VM runs, Lima's serial*.log in its instance directory (`limactl list <vm> --format '{{.Dir}}'`) holds the trace too, and `lever apply` saves it"

// checkLastGuestBoot warns when the previous guest boot did not end with a
// clean shutdown, or when the newest saved console logs hold a kernel panic.
// A warning only: the guest has already recovered.
func checkLastGuestBoot(backendName, guestLogDir string, p guestBootProbe, perr error) checkResult {
	const name = "last guest boot"
	if backendName != config.BackendLima {
		return checkResult{name, true, "not applicable (OrbStack: the kernel is OrbStack's)", ""}
	}
	set := lima.LatestGuestLogSet(guestLogDir)
	var saved, panicLine string
	if set != "" {
		saved = "newest saved console logs: " + set
		if t := lima.GuestLogSetTime(set); !t.IsZero() {
			saved += " (saved " + t.Format(time.RFC3339) + ")"
		}
		panicLine = lima.PanicLineIn(set)
	}
	var detail string
	unclean := false
	switch {
	case perr != nil:
		detail = "not checked (the guest did not answer: " + firstLine(perr.Error()) + ")"
	case !p.sudo:
		detail = "not checked (sudo in the guest refused to read the journal)"
	case p.last.IsZero():
		detail = "the guest journal shows no earlier boot"
	case p.clean:
		detail = "the previous boot ended with a clean shutdown (last journal entry " + p.last.Format(time.RFC3339) + ")"
	default:
		unclean = true
		detail = "the previous boot ended without a clean shutdown (last journal entry " + p.last.Format(time.RFC3339) + ")"
	}
	if saved != "" {
		detail += "; " + saved
	}
	if panicLine != "" {
		detail += "; it holds a kernel panic: " + panicLine
	}
	if !unclean && panicLine == "" {
		return checkResult{name, true, detail, ""}
	}
	fix := guestBootFixPanic
	if set == "" {
		fix = "lever saves Lima's console logs before each VM start (to " + guestLogDir + "); " + guestBootFixPanic
	}
	return warnResult(name, detail, fix)
}
