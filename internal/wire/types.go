package wire

// Request/response bodies of the broker's HTTP routes. Each type is the ONE
// declaration of its JSON shape: the broker decodes/encodes it and the agent,
// captool and cli marshal/decode the very same type. Responses whose payload
// is a host-side runtime type (scion agents, inbox events) stay declared in
// internal/broker, because this package must remain a leaf the jail binary
// can link without pulling in host-only code.

// ---- identity (jail listener) ----

// EnrolRequest is the body of POST /enrol (no client cert; ticket-authorised).
type EnrolRequest struct {
	Ticket string `json:"ticket"`
	CSR    string `json:"csr"` // PEM CSR; CN must equal the ticket's worker
}

// EnrolResponse carries the signed client cert PEM.
type EnrolResponse struct {
	Cert string `json:"cert"`
}

// RenewRequest carries a fresh CSR (new keypair). Its CN is IGNORED; the renewed
// cert always carries the caller's authenticated CN.
type RenewRequest struct {
	CSR string `json:"csr"`
}

// RenewResponse carries the renewed client cert PEM.
type RenewResponse struct {
	Cert string `json:"cert"`
}

// ProvisionRequest is the body of POST /provision (manager only).
type ProvisionRequest struct {
	Worker string `json:"worker"`
}

// ProvisionResponse carries the one-time enrolment ticket.
type ProvisionResponse struct {
	Ticket string `json:"ticket"`
}

// ---- capabilities ----

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

// ---- workers and messaging (manager ⇄ broker) ----

// WorkerStartRequest is the body of POST /worker/start.
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
// (/worker/start|stop|suspend|resume).
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

// ---- operator directives: agent side (jail listener) ----

// DirectiveIDRequest is the body of POST /directive/consume and /directive/check.
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

// ---- operator directives: admin side (UDS channel) ----

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

// ---- admin (loopback listener) ----

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
// current epoch, so captool can verify tokens independently + check freshness.
type RegisterResponse struct {
	PublicKey string `json:"public_key"`
	Epoch     int    `json:"epoch"`
}

// EpochResponse reports the broker's current minimum acceptable token epoch,
// plus the serving process's identity: the binary version it runs and a
// digest of the broker-relevant configuration it was started with. apply's
// broker-reuse shortcut compares these against its own expectation and
// restarts the broker on mismatch — a broker predating these fields
// reports them empty, which callers treat as a mismatch.
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
