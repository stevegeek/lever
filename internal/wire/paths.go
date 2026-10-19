package wire

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
	PathWorkerRecycle    = "/worker/recycle"
	PathWorkerList       = "/worker/list"
	PathMsgSend          = "/msg/send"
	PathMsgList          = "/msg/list"
	PathMsgRecipients    = "/msg/recipients"
	PathDirectiveConsume = "/directive/consume"
	PathDirectiveCheck   = "/directive/check"
	PathDirectivePreview = "/directive/preview"
	PathMessageVerify    = "/message/verify"
	// PathChatVerify is an alias of PathMessageVerify kept for older agent
	// images.
	PathChatVerify = "/chat/verify"
	// PathContacts lists the contacts an agent may message (remote.agent_messages).
	PathContacts = "/contacts"
	// PathContactMessage authorizes one message to a contact and records it.
	PathContactMessage = "/contact/message"
	// PathFilesList lists an agent's file exchange (remote.files).
	PathFilesList = "/files/list"
	// PathFilesShare records one file an agent shares with a login.
	PathFilesShare = "/files/share"
	// PathVersion answers the host lever's version and Jail ABI to any live
	// agent, and nothing else.
	PathVersion = "/version"
)

// Admin (loopback) listener routes.
const (
	PathRegister  = "/register"
	PathEpoch     = "/epoch"
	PathBumpEpoch = "/bump-epoch"
	PathRevoke    = "/revoke"
	PathBootstrap = "/bootstrap"
	// PathWorkerTicket mints and stages a worker's enrolment ticket through
	// the broker's guest channel.
	PathWorkerTicket = "/worker-ticket"
)

// Operator note (UDS) channel route: `lever msg send`.
const PathOperatorNote = "/operator/note"

// PathOperatorAgentMessagesMatch asks which agent rows of a contact's history
// the agent ledger recorded. It is served only on the 0600 operator socket,
// never the jail or admin listener, because it binds ledger records.
const PathOperatorAgentMessagesMatch = "/operator/agent-messages/match"

// Operator-directive (UDS) admin channel routes.
const (
	PathDirectiveSend     = "/directive/send"
	PathDirectiveResolve  = "/directive/resolve"
	PathDirectiveList     = "/directive/list"
	PathDirectiveRevoke   = "/directive/revoke"
	PathDirectiveSelftest = "/directive/selftest"
)
