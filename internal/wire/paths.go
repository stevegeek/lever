package wire

// Route paths served by the broker. The broker registers each on its mux and
// every client (agent, captool, cli) builds its URL from the same constant, so
// a path can never drift between a producer and a consumer.

// Jail (mTLS) listener routes.
const (
	PathEnrol            = "/enrol"
	PathRenew            = "/renew"
	PathRequest          = "/request"
	PathProvision        = "/provision"
	PathTools            = "/tools"
	PathWorkerStart      = "/worker/start"
	PathWorkerStop       = "/worker/stop"
	PathWorkerSuspend    = "/worker/suspend"
	PathWorkerResume     = "/worker/resume"
	PathWorkerList       = "/worker/list"
	PathMsgSend          = "/msg/send"
	PathMsgList          = "/msg/list"
	PathDirectiveConsume = "/directive/consume"
	PathDirectiveCheck   = "/directive/check"
)

// Admin (loopback) listener routes.
const (
	PathRegister  = "/register"
	PathEpoch     = "/epoch"
	PathBumpEpoch = "/bump-epoch"
	PathRevoke    = "/revoke"
	PathBootstrap = "/bootstrap"
)

// Operator-directive (UDS) admin channel routes.
const (
	PathDirectiveSend     = "/directive/send"
	PathDirectiveResolve  = "/directive/resolve"
	PathDirectiveList     = "/directive/list"
	PathDirectiveRevoke   = "/directive/revoke"
	PathDirectiveSelftest = "/directive/selftest"
)
