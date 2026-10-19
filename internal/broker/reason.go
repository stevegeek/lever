package broker

import "net/http"

// PathOperatorWorkerResume is `lever worker resume`'s route on the 0600
// operator socket. It lives here, not in internal/wire: wire is in the jail's
// import graph, and this route is host-only.
const PathOperatorWorkerResume = "/operator/worker/resume"

// The other host-only worker routes of the operator socket, for `lever worker
// list|start|recycle|suspend|stop`.
const (
	PathOperatorWorkerList    = "/operator/worker/list"
	PathOperatorWorkerStart   = "/operator/worker/start"
	PathOperatorWorkerRecycle = "/operator/worker/recycle"
	PathOperatorWorkerSuspend = "/operator/worker/suspend"
	PathOperatorWorkerStop    = "/operator/worker/stop"
	// PathOperatorWorkerNewSession is `lever worker new-session`.
	PathOperatorWorkerNewSession = "/operator/worker/new-session"
)

// ReasonHeader carries a machine-readable reason on a worker refusal, so a
// host program can branch on it without reading the text (which can name a
// phase or a path). The body keeps its human text unchanged. PhaseHeader names
// the worker's phase when the refusal depends on it.
const (
	ReasonHeader = "X-Lever-Reason"
	PhaseHeader  = "X-Lever-Phase"
)

// Reasons. They are a documented interface (docs-site CLI reference): add new
// ones, never rename one.
const (
	ReasonBadRequest         = "bad-request"         // 400: the body did not decode
	ReasonNotAWorker         = "not-a-worker"        // 403: not a declared worker (the manager included)
	ReasonRevoked            = "revoked"             // 403: the worker's identity is revoked
	ReasonRuntimeUnavailable = "runtime-unavailable" // 502: the broker has no scion runtime
	ReasonBusy               = "busy"                // 503: another start, resume, stop, wake or heal of the worker is under way
	ReasonWorkerCap          = "worker-cap"          // 409: too many workers are running
	ReasonRoleMismatch       = "role-mismatch"       // 409: the record's stored role is not the configured one
	ReasonNoRecord           = "no-record"           // 404 (resume) or 409 (note, wake): the hub has no record of the worker
	ReasonNotRunning         = "not-running"         // 409 (note): the worker is not running; PhaseHeader names its phase
	ReasonNotAsleep          = "not-asleep"          // 409 (wake): the worker is neither suspended nor stopped
	ReasonStoppedByOperator  = "stopped-by-operator" // 409 (wake): a contact may not undo the operator's stop
	ReasonWorkspaceUnsafe    = "workspace-unsafe"    // 403: the workspace or a shared folder is reached through a link or overlaps a protected dir
	ReasonWorkspaceMissing   = "workspace-missing"   // 409: the worker's workspace directory no longer exists
	ReasonSharedAccess       = "shared-access"       // 403: the record holds shared-folder access the config has withdrawn
	ReasonHubRefused         = "hub-refused"         // 409: the hub refused the resume; the record is kept
	ReasonNotLive            = "not-live"            // 502: resumed, but the worker did not come live
	ReasonRuntimeError       = "runtime-error"       // 502 or 500: a runtime or staging error; the record is kept
	ReasonBadTask            = "bad-task"            // 400: the task is flag-shaped
	ReasonTaskTooLong        = "task-too-long"       // 413: the task is over scion's command-line budget
	ReasonTaskFixed          = "task-fixed"          // 409 (start): a task was given, but the record exists and its task is fixed
	ReasonConfigFile         = "config-file"         // 500: the worker's instructions_file or task_file cannot be read or is invalid
	ReasonNotRecyclable      = "not-recyclable"      // 403 (recycle): the worker does not set recyclable
	ReasonNotStopped         = "not-stopped"         // 409 (recycle): the record is not suspended, stopped or error, or its container is live
	ReasonRateLimited        = "rate-limited"        // 429 (recycle): the worker was recycled less than a minute ago
)

// refuse writes an error answer with its machine-readable reason. The text is
// for people; the reason is for programs.
func refuse(w http.ResponseWriter, reason, msg string, code int) {
	w.Header().Set(ReasonHeader, reason)
	http.Error(w, msg, code)
}

// refusePhase is refuse with the worker's phase named in PhaseHeader. phase
// must be a known label (phaseOf's), never hub text. The one label that is not
// a single token goes out as one.
func refusePhase(w http.ResponseWriter, reason, phase, msg string, code int) {
	if phase == phaseRunningContainerDown {
		phase = PhaseRunningContainerDownToken
	}
	if phase != "" {
		w.Header().Set(PhaseHeader, phase)
	}
	refuse(w, reason, msg, code)
}

// PhaseRunningContainerDownToken is PhaseHeader's spelling of a record the
// hub reports running while its container is down.
const PhaseRunningContainerDownToken = "running-container-down"
