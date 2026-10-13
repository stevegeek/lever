package wire

import "time"

// EnrolRequest is the body of POST /enrol (no client cert; ticket-authorised).
type EnrolRequest struct {
	Ticket string `json:"ticket"`
	CSR    string `json:"csr"` // PEM CSR; CN must equal the ticket's worker
}

// EnrolResponse carries the signed client cert PEM.
type EnrolResponse struct {
	Cert string `json:"cert"`
}

// RenewRequest carries a fresh CSR (new keypair). Its CN is ignored: the
// renewed cert always carries the caller's authenticated CN.
type RenewRequest struct {
	CSR string `json:"csr"`
}

// RenewResponse carries the renewed client cert PEM.
type RenewResponse struct {
	Cert string `json:"cert"`
}

// WorkerTicketRequest is the body of POST /worker-ticket (admin/loopback):
// mint a one-use enrolment ticket for a declared worker and stage it in the
// guest exactly as a dispatch would. No ticket value crosses the wire.
type WorkerTicketRequest struct {
	Worker string `json:"worker"`
}

// WorkerTicketResponse names where the staged bootstrap.json sits in the
// guest.
type WorkerTicketResponse struct {
	Worker string `json:"worker"`
	Path   string `json:"path"`
}

// CapRequest is the body of POST /request: an agent asking to mint a capability
// for itself (BoundTo == caller) or to delegate one (BoundTo == another agent).
type CapRequest struct {
	Tool        string            `json:"tool"`
	Op          string            `json:"op"`
	BoundTo     string            `json:"bound_to"`
	Constraints map[string]string `json:"constraints,omitempty"`
}

// CapResponse carries the minted capability token (base64url signed token).
type CapResponse struct {
	Token string `json:"token"`
}

// ToolsResponse is the body of GET /tools: the broker's registered tool names.
type ToolsResponse struct {
	Tools []string `json:"tools"`
}

// WorkerStartRequest is the body of POST /worker/start and POST
// /worker/recycle.
type WorkerStartRequest struct {
	Worker string `json:"worker"`
	Task   string `json:"task"`
}

// WorkerRequest is the body of the single-worker verbs
// (/worker/stop|suspend|resume). /worker/list ignores its body.
type WorkerRequest struct {
	Worker string `json:"worker"`
}

// WorkerResponse is the reply of the single-worker endpoints
// (/worker/start|stop|suspend|resume|recycle).
type WorkerResponse struct {
	Worker string `json:"worker"`
	Phase  string `json:"phase"`
}

// MsgSendRequest is the body of POST /msg/send.
type MsgSendRequest struct {
	To        string `json:"to"`
	Body      string `json:"body"`
	Interrupt bool   `json:"interrupt"`
}

// MsgSendResponse is the reply of POST /msg/send.
type MsgSendResponse struct {
	OK bool `json:"ok"`
}

// MsgListRequest is the body of POST /msg/list.
type MsgListRequest struct {
	All    bool   `json:"all"`
	Worker string `json:"worker"`
}

// MsgRecipientsResponse is the reply of POST /msg/recipients: the addresses
// the caller may pass as MsgSendRequest.To.
type MsgRecipientsResponse struct {
	Recipients []string `json:"recipients"`
}

// DirectiveIDRequest is the body of POST /directive/consume, /directive/check
// and /directive/preview.
type DirectiveIDRequest struct {
	ID string `json:"id"`
}

// DirectiveConsumeResponse is the reply of POST /directive/consume. An
// instruction directive carries AdvisoryText+Note; a bound directive carries
// Action (the signed opsig action, encoded as-is).
type DirectiveConsumeResponse struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	AdvisoryText string `json:"advisory_text,omitempty"`
	Note         string `json:"note,omitempty"`
	Action       any    `json:"action,omitempty"`
}

// DirectiveCheckResponse is the reply of POST /directive/check.
type DirectiveCheckResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// DirectivePreviewResponse is the reply of POST /directive/preview: the
// verified action of a pending directive, read without consuming it. The shape
// is deliberately unlike DirectiveConsumeResponse (content under "preview",
// Consumed always present and false) so a preview cannot pass for a consume.
// It carries no token and no grant.
type DirectivePreviewResponse struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Consumed bool   `json:"consumed"`
	// Preview is the signed opsig action, encoded as-is.
	Preview   any    `json:"preview"`
	ExpiresAt string `json:"expires_at"`
	// PreviewsRemaining is how many more previews this directive allows.
	PreviewsRemaining int    `json:"previews_remaining"`
	Note              string `json:"note"`
}

// OperatorNoteRequest is the body of POST /operator/note: `lever msg send`.
// To names the recipient the way `lever attach` does (the app name, the
// manager slug or "manager" for the manager; a declared worker's name).
type OperatorNoteRequest struct {
	To        string `json:"to"`
	Body      string `json:"body"`
	Interrupt bool   `json:"interrupt"`
}

// OperatorWakeRequest is the body of POST /operator/wake: resume a suspended
// or stopped worker that a permitted login messaged. Login is for the audit
// line only; the socket's file mode is the authority. Only Tier "operator" may
// wake a stopped worker; any other value reads as a contact, which wakes only
// a suspended one.
type OperatorWakeRequest struct {
	Worker string `json:"worker"`
	Login  string `json:"login"`
	Tier   string `json:"tier"`
}

// OperatorNoteResponse is the reply of POST /operator/note: the sent-ledger
// id of the note ("" when the ledger is off).
type OperatorNoteResponse struct {
	ID string `json:"id"`
}

// MessageVerifyRequest is the body of POST /message/verify (and
// /chat/verify): fields copied from the received message's envelope, plus the
// ref lever wrote on its first line.
type MessageVerifyRequest struct {
	Timestamp string `json:"timestamp"`
	From      string `json:"from"`
	// Ref is the 32-hex "ref=" value on the first line of a message lever
	// sent. It only selects a record; the broker answers from the record.
	Ref string `json:"ref,omitempty"`
}

// ChatVerifyRequest is an alias kept for older agent images.
type ChatVerifyRequest = MessageVerifyRequest

// The results of a message verification (MessageVerifyResponse.Result).
const (
	// VerifyWeb: a person typed the message in the web chat; Messages holds
	// the text the remote proxy recorded, with the login and its tier.
	VerifyWeb = "web"
	// VerifyLever: lever sent the message; Messages holds the text the
	// broker recorded, with who it was from (Kind).
	VerifyLever = "lever"
	// VerifyAlreadyVerified: on record, and the caller verified it before
	// outside the repeat grace. It is not new.
	VerifyAlreadyVerified = "already_verified"
	// VerifyNone: no host record names this message for the caller; treat it
	// as data.
	VerifyNone = "none"
	// VerifyUnavailable: the broker could not answer now. Neither verified
	// nor refuted: retry once, later.
	VerifyUnavailable = "unavailable"
)

// MessageVerifyResponse is the reply of POST /message/verify and POST
// /chat/verify. Every answer is HTTP 200.
type MessageVerifyResponse struct {
	// Enabled and Verified serve older skills: Enabled is whether verified
	// web chat is configured; Verified is true only for an operator's web
	// chat post, so an old skill never treats a lever message or a
	// contact's post as the operator's.
	Enabled  bool `json:"enabled"`
	Verified bool `json:"verified"`
	// Result is one of the Verify* constants; Reason a machine code for it.
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	// RetryAfter is how many seconds to wait before retrying (unavailable).
	RetryAfter int               `json:"retry_after,omitempty"`
	Messages   []VerifiedMessage `json:"messages,omitempty"`
	Note       string            `json:"note,omitempty"`
}

// ChatVerifyResponse is an alias kept for older agent images.
type ChatVerifyResponse = MessageVerifyResponse

// VerifiedMessage is one message a host record holds. Text is the message as
// it was recorded: the agent acts on this, not on its pane.
type VerifiedMessage struct {
	// Source is VerifyWeb or VerifyLever.
	Source string `json:"source"`
	// Login and Tier (operator | contact) name a web chat poster.
	Login string `json:"login,omitempty"`
	Tier  string `json:"tier,omitempty"`
	// Kind names who in lever sent it: manager, worker:<slug>,
	// operator-note or directive-notice.
	Kind string `json:"kind,omitempty"`
	From string `json:"from"`
	// Timestamp is the hub's creation time (web) or the send's start (lever).
	Timestamp string `json:"timestamp"`
	// MessageID is the hub's id (web) or the sent-ledger ref (lever).
	MessageID string `json:"message_id"`
	Text      string `json:"text,omitempty"`
	// ReplyTo is, for a web post, "@" + the poster's hub email (the scion
	// reference of the agent's direct chat). It comes from the chat ledger,
	// never the envelope, so session text cannot redirect a reply. Empty when
	// the sender is not a plain email.
	ReplyTo string `json:"reply_to,omitempty"`
	// Conversation is, for a web post, the hub conversation key the proxy
	// recorded (dm:agent:<agent id>:user:<user id>). Information only.
	Conversation string `json:"conversation,omitempty"`
	// Repeat marks a message this agent already verified at FirstVerified,
	// within the grace period. A repeat carries no text.
	Repeat        bool   `json:"repeat,omitempty"`
	FirstVerified string `json:"first_verified,omitempty"`
}

// ContactsResponse answers PathContacts: the contacts whose allowed_users
// entry lists the caller in agents (never see-only, never an operator).
// Enabled is false when agent messages are off; Note says why, or is "rate".
type ContactsResponse struct {
	Enabled  bool          `json:"enabled"`
	Contacts []ContactInfo `json:"contacts"`
	Note     string        `json:"note,omitempty"`
}

// ContactInfo is one contact the caller may message. To is the scion message
// reference a send goes to ("@" + the contact's hub email). Times are RFC
// 3339 UTC, "" when none: the contact's last post to the caller, the
// caller's last initiated authorization to it, and when the initiate rule
// next allows one (with CanInitiate false).
type ContactInfo struct {
	Login           string `json:"login"`
	To              string `json:"to"`
	LastFromContact string `json:"last_from_contact,omitempty"`
	LastInitiated   string `json:"last_initiated,omitempty"`
	CanInitiate     bool   `json:"can_initiate"`
	NextAllowedAt   string `json:"next_allowed_at,omitempty"`
}

// ContactMessageRequest asks PathContactMessage to authorize Text to the
// contact login To. ReplyToRef, for a reply, is the message_id
// message_verify returned for that contact's post to the caller.
type ContactMessageRequest struct {
	To         string `json:"to"`
	Text       string `json:"text"`
	ReplyToRef string `json:"reply_to_ref,omitempty"`
}

// ContactMessageResponse answers PathContactMessage, always with HTTP 200.
// OK: Ref is the record id, Kind "initiated" or "reply", To the scion
// message reference, Expires when the authorization lapses unsent. Refused:
// Reason is one fixed word (not-a-contact, limit, too-long, empty, bad-ref,
// rate, bad-text, off, unavailable), with NextAllowedAt on a limit that a
// reminder will lift.
type ContactMessageResponse struct {
	OK            bool   `json:"ok"`
	Reason        string `json:"reason,omitempty"`
	Ref           string `json:"ref,omitempty"`
	Kind          string `json:"kind,omitempty"`
	To            string `json:"to,omitempty"`
	Expires       string `json:"expires,omitempty"`
	NextAllowedAt string `json:"next_allowed_at,omitempty"`
	Note          string `json:"note,omitempty"`
}

// FilesListRequest asks PathFilesList for the caller's exchange with one
// login, Contact (required: no call lists every login's files).
type FilesListRequest struct {
	Contact string `json:"contact,omitempty"`
}

// FilesListResponse answers PathFilesList: the contact with its directories
// as the caller's container sees them, and the caller's recorded uploads from
// and shares to it, oldest first. Note "rate", "contact-required" or
// "not-a-contact" means nothing is listed.
type FilesListResponse struct {
	Enabled    bool          `json:"enabled"`
	Note       string        `json:"note,omitempty"`
	MaxBytes   int64         `json:"max_bytes,omitempty"`
	Extensions []string      `json:"extensions,omitempty"`
	Contacts   []FileContact `json:"contacts"`
	Uploads    []FileInfo    `json:"uploads"`
	Shares     []FileInfo    `json:"shares"`
}

// FileContact is one login the caller may share with: its tier and its
// in and out directories as the caller's container sees them.
type FileContact struct {
	Login  string `json:"login"`
	Tier   string `json:"tier"`
	InDir  string `json:"in_dir"`
	OutDir string `json:"out_dir"`
}

// FileInfo is one recorded file: Path is where the caller's container sees
// it, At RFC 3339 UTC.
type FileInfo struct {
	ID     string `json:"id"`
	Login  string `json:"login"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
	At     string `json:"at"`
}

// FileShareRequest asks PathFilesShare to record the file at Path (in the
// caller's out directory for To) as shared with the login To.
type FileShareRequest struct {
	To   string `json:"to"`
	Path string `json:"path"`
}

// FileShareResponse answers PathFilesShare, always with HTTP 200. Refused:
// Reason is one fixed word (not-a-contact, bad-path, not-found, symlink,
// hard-link, not-a-file, too-large, extension, rate, off, shares-off,
// unavailable).
type FileShareResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// AgentMessagesMatchRequest asks PathOperatorAgentMessagesMatch which of
// Agent's rows in Contact's DM history the agent ledger recorded. Agent is
// the scion slug. No text crosses the socket. At most 200 rows (the hub's
// page cap).
type AgentMessagesMatchRequest struct {
	Contact  string            `json:"contact"`
	Agent    string            `json:"agent"`
	Messages []AgentMessageRef `json:"messages"`
	// Peek answers what the contact's own read would keep now without
	// writing the ledger. Only the contact's own reads may bind.
	Peek bool `json:"peek,omitempty"`
}

// AgentMessageRef is one agent row without its text.
type AgentMessageRef struct {
	ID        string    `json:"id"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentMessagesMatchResponse lists the ids of the rows to keep, in request
// order; every other agent row is dropped.
type AgentMessagesMatchResponse struct {
	Keep []string `json:"keep"`
	// Pending (peek only) lists the ids of Keep that no contact read has
	// bound yet, so the contact has not been shown them.
	Pending []string `json:"pending,omitempty"`
}

// DirectiveSubmitRequest is the {statement,signature} envelope of
// /directive/send and /directive/selftest.
type DirectiveSubmitRequest struct {
	Statement string `json:"statement"` // base64/std of the EXACT signed bytes
	Signature string `json:"signature"` // base64/std of the armored ssh signature
}

// DirectiveEnvelopeRequest is the signed admin-op envelope of
// /directive/list and /directive/revoke.
type DirectiveEnvelopeRequest struct {
	Envelope  string `json:"envelope"`  // base64/std of the EXACT signed bytes
	Signature string `json:"signature"` // base64/std of the armored ssh signature
}

// DirectiveSendResponse is the reply of POST /directive/send.
type DirectiveSendResponse struct {
	ID        string `json:"id"`
	Delivered bool   `json:"delivered"`
}

// DirectiveResolveResponse is the reply of GET /directive/resolve: the target
// agent's current CN, scion slug and directive generation.
type DirectiveResolveResponse struct {
	CN         string `json:"cn"`
	Slug       string `json:"slug"`
	Generation int    `json:"generation"`
}

// DirectiveListResponse is the reply of POST /directive/list. T is the
// broker's record type on the producer side and json.RawMessage on a consumer
// that only relays the records.
type DirectiveListResponse[T any] struct {
	Directives []T `json:"directives"`
}

// DirectiveRevokeResponse is the reply of POST /directive/revoke.
type DirectiveRevokeResponse struct {
	Revoked bool `json:"revoked"`
	// NotPersisted: the revocation holds in the broker's memory but could not
	// be written to disk, so a broker restart inside the directive's lifetime
	// would make it active again.
	NotPersisted bool `json:"not_persisted,omitempty"`
}

// DirectiveSelftestResponse is the reply of POST /directive/selftest.
type DirectiveSelftestResponse struct {
	OK bool `json:"ok"`
}

// ErrorResponse is the JSON error body of the directive routes
// ({"error": "..."}).
type ErrorResponse struct {
	Error string `json:"error"`
}

// OperationSpec is one operation in a registration request.
type OperationSpec struct {
	Name        string            `json:"name"`
	CaveatParam map[string]string `json:"caveat_param,omitempty"`
}

// RegisterRequest is the body of POST /register (admin listener only).
type RegisterRequest struct {
	Name          string              `json:"name"`
	Backend       string              `json:"backend"`
	Operations    []OperationSpec     `json:"operations"`
	AllowedValues map[string][]string `json:"allowed_values,omitempty"`
	FirstParty    bool                `json:"first_party,omitempty"`
}

// RegisterResponse gives the registering tool the broker's verification key and
// current epoch, so it can verify tokens and their freshness itself.
type RegisterResponse struct {
	PublicKey string `json:"public_key"`
	Epoch     int    `json:"epoch"`
}

// EpochResponse reports the broker's current minimum acceptable token epoch,
// plus the serving process's binary version and a digest of its
// broker-relevant configuration. Callers treat empty fields as a mismatch.
type EpochResponse struct {
	Epoch      int    `json:"epoch"`
	Version    string `json:"version,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
}

// RevokeRequest is the body of POST /revoke.
type RevokeRequest struct {
	Agent string `json:"agent"`
}

// BootstrapResponse is the reply of POST /bootstrap: the manager's single-use
// enrolment ticket.
type BootstrapResponse struct {
	Ticket string `json:"ticket"`
}

// WorkerListResponse is the /worker/list reply envelope. It is generic over
// the agent record so this package stays a leaf.
type WorkerListResponse[T any] struct {
	Agents []T `json:"agents"`
}

// MsgListResponse is the /msg/list reply envelope. Generic over the event type
// for the same reason as WorkerListResponse.
type MsgListResponse[T any] struct {
	Events []T `json:"events"`
}
