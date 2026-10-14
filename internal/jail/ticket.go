package jail

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// A worker's enrolment ticket never touches the instance tree: the manager
// mounts the whole tree, so a ticket staged there could be redeemed by a
// compromised manager to take the worker's identity. Tickets are staged in a
// per-worker directory under the run user's XDG_RUNTIME_DIR in the guest (a
// 0700 tmpfs), which only the worker's own container mounts, read-only (see
// broker.WorkerSpec.TicketDir).
//
// As with StageHubToken, the payload rides the child's stdin behind a fixed
// script, so it never appears on a host argv. The worker name is the one
// positional, config-validated and re-checked here, never a request value.
// Both scripts refuse to run without XDG_RUNTIME_DIR rather than fall back
// to a shared path.

// workerTicketRoot must match WorkerTicketDir.
const workerTicketRoot = "$XDG_RUNTIME_DIR/lever/tickets"

// workerTicketFile is the name lever-agent boot reads (wire.Stage's deposit).
const workerTicketFile = "bootstrap.json"

// stageWorkerTicketScript removes the old file first so the write never
// follows a planted link and the mode is set by umask, not inherited.
const stageWorkerTicketScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; umask 077; d="` + workerTicketRoot + `/$1"; mkdir -p "$d" && rm -f "$d/` + workerTicketFile + `" && cat > "$d/` + workerTicketFile + `"`

const removeWorkerTicketScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; rm -rf "` + workerTicketRoot + `/$1"`

// workerNameRE mirrors config.nameRE: the name becomes a path component of
// the staging script and a positional on the host argv.
var workerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func checkWorkerName(worker string) error {
	if !workerNameRE.MatchString(worker) {
		return fmt.Errorf("worker name %q is not a valid ticket directory name", worker)
	}
	return nil
}

// WorkerTicketDir is the guest path of worker's ticket directory, the source
// of the read-only volume the worker's container mounts.
func WorkerTicketDir(uid, worker string) string {
	return "/run/user/" + uid + "/lever/tickets/" + worker
}

// WorkerTicketFile is the guest path of the staged bootstrap.json.
func WorkerTicketFile(uid, worker string) string {
	return WorkerTicketDir(uid, worker) + "/" + workerTicketFile
}

// StageWorkerTicket writes payload (a marshalled wire.Bootstrap) as worker's
// bootstrap.json, mode 0600, through the jail runner r. Call it right before
// every worker start or resume: a spent ticket is useless to the booting
// worker.
func StageWorkerTicket(ctx context.Context, r proc.Runner, worker string, payload []byte) error {
	if err := checkWorkerName(worker); err != nil {
		return fmt.Errorf("staging worker ticket: %w", err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("staging worker ticket for %q: empty payload", worker)
	}
	res, err := r.RunStdin(ctx, strings.NewReader(string(payload)), nil, "sh", "-c", stageWorkerTicketScript, "_", worker)
	if err != nil {
		return fmt.Errorf("staging worker ticket for %q in the jail: %w: %s", worker, err, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// RemoveWorkerTicket deletes worker's staged ticket directory in the jail.
// An absent directory is not an error.
func RemoveWorkerTicket(ctx context.Context, r proc.Runner, worker string) error {
	if err := checkWorkerName(worker); err != nil {
		return fmt.Errorf("removing worker ticket: %w", err)
	}
	res, err := r.Run(ctx, nil, "sh", "-c", removeWorkerTicketScript, "_", worker)
	if err != nil {
		return fmt.Errorf("removing worker ticket for %q in the jail: %w: %s", worker, err, strings.TrimSpace(res.Stderr))
	}
	return nil
}
