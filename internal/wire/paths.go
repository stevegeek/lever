package wire

// Route paths served by the broker. The broker registers each on its mux and
// every client (agent, captool, cli) builds its URL from the same constant, so
// a path can never drift between a producer and a consumer.

// Jail (mTLS) listener routes.
const (
	PathEnrol            = "/enrol"
	PathRenew            = "/renew"
	PathRequest          = "/request"
	PathTools            = "/tools"
	PathWorkerStart      = "/worker/start"
	PathWorkerStop       = "/worker/stop"
	PathWorkerSuspend    = "/worker/suspend"
	PathWorkerResume     = "/worker/resume"
	PathWorkerList       = "/worker/list"
	PathMsgSend          = "/msg/send"
	PathMsgList          = "/msg/list"
	PathMsgRecipients    = "/msg/recipients"
	PathDirectiveConsume = "/directive/consume"
	PathDirectiveCheck   = "/directive/check"
	PathDirectivePreview = "/directive/preview"
	PathMessageVerify    = "/message/verify"
	// PathChatVerify is the 0.27 route agent images of that release post
	// to; it answers like PathMessageVerify.
	PathChatVerify = "/chat/verify"
	// PathContacts lists the contacts an agent may message (remote.agent_messages).
	PathContacts = "/contacts"
	// PathContactMessage authorizes one message to a contact and records it.
	PathContactMessage = "/contact/message"
)

// Admin (loopback) listener routes.
const (
	PathRegister  = "/register"
	PathEpoch     = "/epoch"
	PathBumpEpoch = "/bump-epoch"
	PathRevoke    = "/revoke"
	PathBootstrap = "/bootstrap"
	// PathWorkerTicket mints AND stages a worker's enrolment ticket through
	// the broker's guest channel (the acceptance harness's host-side mint).
	PathWorkerTicket = "/worker-ticket"
)

// Operator note (UDS) channel route: `lever msg send`.
const PathOperatorNote = "/operator/note"

// PathOperatorWake is the remote chat page's wake of a suspended or stopped
// worker, on the same 0600 operator socket (never the admin listener).
const PathOperatorWake = "/operator/wake"

// PathOperatorAgentMessagesMatch is the remote proxy's question, on the same
// 0600 operator socket, which agent rows of a contact's history the agent
// ledger recorded (remote.agent_messages). Never on the jail or admin
// listener: it binds ledger records.
const PathOperatorAgentMessagesMatch = "/operator/agent-messages/match"

// Operator-directive (UDS) admin channel routes.
const (
	PathDirectiveSend     = "/directive/send"
	PathDirectiveResolve  = "/directive/resolve"
	PathDirectiveList     = "/directive/list"
	PathDirectiveRevoke   = "/directive/revoke"
	PathDirectiveSelftest = "/directive/selftest"
)
