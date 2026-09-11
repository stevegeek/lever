package jail

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// A worker's enrolment ticket never touches the instance tree.
//
// The manager mounts the WHOLE tree, so a ticket staged at
// <tree>/<worker dir>/.lever/bootstrap.json sits inside the manager's mount
// for the window between staging and the worker's boot — long enough for a
// compromised manager to redeem it and hold the worker's identity (its
// grants, its inbox, the operator directives addressed to it). So the broker
// stages worker tickets HERE instead: a per-worker directory under the run
// user's XDG_RUNTIME_DIR in the guest (a 0700 tmpfs, gone on reboot), which
// no container mounts by default. The worker alone gets it, read-only, as an
// explicit volume on its own container (see broker.WorkerSpec.TicketDir), so
// the file is visible to exactly one agent.
//
// The transport is the one StageHubToken uses: the payload rides the child's
// stdin behind a fixed guest-side script, so the ticket never appears on a
// host argv. The worker name is the script's single positional — it is
// config-validated (config.nameRE) and re-checked here, never a request value.
// Both scripts refuse to run without XDG_RUNTIME_DIR rather than fall back
// to a shared path; jailEnv sets it on every in-jail command.

// workerTicketRoot is the guest directory holding one subdirectory per
// worker, expanded by the guest shell. WorkerTicketDir is its host-side twin.
const workerTicketRoot = "$XDG_RUNTIME_DIR/lever/tickets"

// workerTicketFile is the file name inside a worker's ticket directory: the
// same name lever-agent boot reads everywhere (wire.Stage's deposit).
const workerTicketFile = "bootstrap.json"

// stageWorkerTicketScript writes stdin to <root>/<$1>/bootstrap.json as a
// fresh 0600 file under 0700 directories. The old file is removed first so
// the write never follows a planted link and the mode is set by umask, not
// inherited.
const stageWorkerTicketScript = `[ -n "$XDG_RUNTIME_DIR" ] || exit 1; umask 077; d="` + workerTicketRoot + `/$1"; mkdir -p "$d" && rm -f "$d/` + workerTicketFile + `" && cat > "$d/` + workerTicketFile + `"`

// removeWorkerTicketScript deletes a worker's ticket directory (idempotent).
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

// WorkerTicketDir is the guest path of worker's ticket directory for the run
// user uid: the source of the read-only volume the worker's container mounts.
func WorkerTicketDir(uid, worker string) string {
	return "/run/user/" + uid + "/lever/tickets/" + worker
}

// WorkerTicketFile is the guest path of the staged bootstrap.json itself
// (the acceptance harness reads it in place; a container sees it under its
// mount target instead).
func WorkerTicketFile(uid, worker string) string {
	return WorkerTicketDir(uid, worker) + "/" + workerTicketFile
}

// StageWorkerTicket writes payload (a marshalled wire.Bootstrap) as worker's
// bootstrap.json in the run user's runtime directory inside the jail, mode
// 0600, through r, the jail runner. The payload travels on stdin; the host
// argv carries only the fixed staging script and the worker name. Call it
// right before EVERY worker start or resume: the previous file is replaced,
// and a spent ticket is useless to the booting worker.
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

// RemoveWorkerTicket deletes worker's staged ticket directory in the jail
// (`lever worker purge`). Idempotent: an absent directory is not an error.
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
