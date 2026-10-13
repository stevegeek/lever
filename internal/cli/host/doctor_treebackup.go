package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/treebackup"
)

const treeBackupRow = "tree backup"

// checkTreeBackup reads only the status files, the snapshot names and the
// newest manifest's header: doctor must stay fast on a large store.
func checkTreeBackup(app *config.App, st state.State, now time.Time) checkResult {
	tb := app.TreeBackup
	if !tb.Enabled {
		return checkResult{name: treeBackupRow, ok: true, detail: "off (tree_backup.enabled)"}
	}
	e := treeEnvFor(app, st)
	if err := e.place.CheckPlacement(); err != nil {
		return checkResult{name: treeBackupRow, detail: err.Error(),
			fix: "move the instance's state directory out of the tree (the tree must not contain lever.yaml's directory)"}
	}
	if err := treebackup.ProbeKernel(); err != nil {
		return checkResult{name: treeBackupRow, detail: err.Error(), fix: "tree backups need Linux 5.8 or later with openat2 allowed"}
	}
	if _, err := os.Lstat(e.place.Store); errors.Is(err, fs.ErrNotExist) {
		return checkResult{name: treeBackupRow, ok: true, detail: "on, no snapshot yet",
			fix: "`lever tree snapshot` (or the next `lever stop`) takes the first one"}
	}
	s, err := treebackup.OpenStore(e.place.Store, false)
	if err != nil {
		return checkResult{name: treeBackupRow, detail: err.Error(), fix: "chmod 700 the store and make sure it belongs to you, or move it aside"}
	}
	defer s.Close()
	infos, strays, err := s.List()
	if err != nil {
		return checkResult{name: treeBackupRow, detail: err.Error()}
	}
	snap, _ := s.ReadSnapshotStatus()
	gc, _ := s.ReadGCStatus()

	var warns []string
	var newest *treebackup.Header
	valid := 0
	for _, in := range infos {
		if in.Header != nil {
			valid++
			newest = in.Header
		}
	}
	if newest == nil {
		warns = append(warns, "no valid snapshot")
	} else {
		if newest.Root.State == treebackup.StateUnreadable {
			if tccSuspect(newest) {
				return checkResult{name: treeBackupRow, detail: fmt.Sprintf("snapshot %s could not open the tree root although its mode allows it: macOS privacy controls (TCC) block lever", newest.ID),
					fix: "grant Full Disk Access to the program that runs lever (the terminal, or launchd's job)"}
			}
			warns = append(warns, "the tree root was made unreadable: the last snapshot holds nothing")
		}
		if !newest.Complete {
			warns = append(warns, "the last snapshot is INCOMPLETE: "+partialSummary(newest))
		}
		if n := newest.Counts.UnreadableNew; n > 0 {
			warns = append(warns, fmt.Sprintf("%d entries became unreadable", n))
		}
		if newest.Quiescent == treebackup.QuiescentVMOff {
			racy := newest.Counts.ByState[treebackup.StateChanged] + newest.Counts.ByState[treebackup.StateUnstable] + newest.Counts.ByState[treebackup.StateListingUnstable]
			if racy > 0 {
				warns = append(warns, fmt.Sprintf("%d entries changed while the VM was off: a host-side writer touches the tree", racy))
			}
		}
		if now.Sub(newest.Created) > 7*24*time.Hour {
			warns = append(warns, "no snapshot in 7 days")
		}
	}
	if n := len(infos) - valid; n > 0 {
		warns = append(warns, fmt.Sprintf("%d manifests with an invalid header (`lever tree verify`)", n))
	}
	switch snap.Result {
	case "failed", "skipped":
		warns = append(warns, fmt.Sprintf("the last snapshot attempt (%s, %s) %s: %s", snap.Trigger, snap.Time.Local().Format("2006-01-02 15:04"), snap.Result, snap.Error))
	}
	switch {
	case gc.Result == treebackup.GCBlocked:
		warns = append(warns, fmt.Sprintf("GC is blocked by %s: nothing is deleted until it is fixed (`lever tree verify`, `lever tree delete`)", gc.Blocked))
	case !gc.Time.IsZero() && now.Sub(gc.Time) > 7*24*time.Hour:
		warns = append(warns, "GC has not run for 7 days")
	case gc.Result == treebackup.GCSkippedBusy && valid > 0:
		warns = append(warns, "the last GC found the store busy")
	}
	if len(strays) > 0 {
		warns = append(warns, fmt.Sprintf("stray files in snapshots/: %s", strings.Join(quoteAll(strays), ", ")))
	}
	if free, err := s.FreeBytes(); err == nil && free < uint64(2*tb.EffectiveMinFree()) {
		warns = append(warns, fmt.Sprintf("only %s free on the store's filesystem (min_free is %s)", humanBytes(int64(free)), humanBytes(tb.EffectiveMinFree())))
	}

	detail := fmt.Sprintf("%d snapshots", valid)
	if newest != nil {
		state := "complete"
		if !newest.Complete {
			state = "incomplete"
		}
		detail = fmt.Sprintf("last snapshot %s ago (%s, %s, %s) · %s", ago(now.Sub(newest.Created)), newest.Trigger, newest.Quiescent, state, detail)
	}
	if !gc.Time.IsZero() {
		detail += fmt.Sprintf(" · %s · GC %s ago", humanBytes(gc.StoreBytes), ago(now.Sub(gc.Time)))
	}
	if len(warns) > 0 {
		return checkResult{name: treeBackupRow, ok: true, detail: detail + "; " + strings.Join(warns, "; "),
			fix: "`lever tree log -n 3` shows the recent snapshots; see the operations guide's tree backups section"}
	}
	return checkResult{name: treeBackupRow, ok: true, detail: detail}
}

// tccSuspect: on darwin an EPERM on a root whose owner bits allow reading
// and searching is the privacy layer, not a chmod by an agent.
func tccSuspect(h *treebackup.Header) bool {
	return runtime.GOOS == "darwin" && h.Root.HasMeta && h.Root.Mode&0o500 == 0o500
}

func partialSummary(h *treebackup.Header) string {
	var parts []string
	for _, p := range h.Partial {
		at := "the tree root"
		if p.HasAt {
			at = treebackup.Q(p.At)
		}
		parts = append(parts, fmt.Sprintf("%s(%s) at %s (%d)", p.State, p.Reason, at, p.Count))
	}
	if h.Root.Listing == treebackup.ListingUnreadable {
		parts = append(parts, "the tree root cannot be listed")
	}
	for _, f := range h.Flood {
		at := "the tree root"
		if f.HasAt {
			at = treebackup.Q(f.At)
		}
		parts = append(parts, fmt.Sprintf("most names in %s (%d)", at, f.Count))
	}
	if len(parts) == 0 {
		return "see `lever tree log -n 1`"
	}
	return strings.Join(parts, ", ")
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = treebackup.Q([]byte(n))
	}
	return out
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
