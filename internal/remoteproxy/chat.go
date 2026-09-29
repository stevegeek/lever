package remoteproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// Verified web chat: the proxy records every chat post it forwarded for a
// verified login, so an agent can later ask the broker whether a message it
// received took this path (see package chatledger).
//
// Only one route is recorded: a post into an agent DM,
// POST /api/v1/chat/conversations/dm:agent:<agentID>:user:<userID>/messages,
// answered 201. That is the web chat. Everything else — a topic thread, the
// quick-message dialog (/api/v1/agents/{id}/message, whose timestamp the
// client may choose), an edit, a 200 idempotency replay, text typed into an
// attached terminal — is not recorded, so it never verifies. That is the safe
// direction: an unrecorded message stays an ordinary unverified turn.

const chatConversationsPrefix = "/api/v1/chat/conversations/"

// maxChatResponse bounds how much of the hub's answer the proxy buffers to
// read it. The answer echoes a message of at most 16000 characters (scion's
// messages.MaxMessageLength) plus its metadata.
const maxChatResponse = 1 << 20

// chatDMAgent returns the conversation key and its agent id when p is the
// send route of an agent DM, else ok=false. p is the decoded path.
func chatDMAgent(method, p string) (key, agentID string, ok bool) {
	if method != http.MethodPost || !strings.HasPrefix(p, chatConversationsPrefix) {
		return "", "", false
	}
	key, ok = strings.CutSuffix(strings.TrimPrefix(p, chatConversationsPrefix), "/messages")
	if !ok || strings.Contains(key, "/") {
		return "", "", false
	}
	parts := strings.Split(key, ":")
	if len(parts) != 5 || parts[0] != "dm" || parts[1] != "agent" || parts[3] != "user" || parts[2] == "" || parts[4] == "" {
		return "", "", false
	}
	return key, parts[2], true
}

// chatSendResponse is the part of scion's chatMessageResponse
// (pkg/hub/handlers_chat_v2.go) the ledger keeps.
type chatSendResponse struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Sender    string    `json:"sender"`
	CreatedAt time.Time `json:"createdAt"`
}

// recordChat appends the ledger entry for resp when it answers a recorded
// chat post from a verified login. It reads the body and puts back an
// identical copy, so the client gets the hub's answer unchanged. A failure to
// record is reported through warn and never fails the request: the message
// is delivered already, and an unrecorded message only fails to verify.
func recordChat(resp *http.Response, login, tier string, ledger func(chatledger.Entry) error, warn func(error)) {
	if ledger == nil || login == "" || resp.StatusCode != http.StatusCreated || resp.Request == nil {
		return
	}
	key, agentID, ok := chatDMAgent(resp.Request.Method, resp.Request.URL.Path)
	if !ok {
		return
	}
	if resp.Header.Get("Content-Encoding") != "" {
		warn(errChat("the hub answered a chat post with an encoded body; not recorded"))
		return
	}
	// The client gets the bytes read here followed by the rest of the
	// original stream, so a read error or an oversized answer never cuts
	// what reaches the browser.
	orig := resp.Body
	body, err := io.ReadAll(io.LimitReader(orig, maxChatResponse+1))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), orig), orig}
	if err != nil {
		warn(errChat("reading the hub's answer to a chat post: " + err.Error()))
		return
	}
	if len(body) > maxChatResponse {
		warn(errChat("the hub's answer to a chat post is too large; not recorded"))
		return
	}
	var sent chatSendResponse
	if err := json.Unmarshal(body, &sent); err != nil || sent.ID == "" || sent.Sender == "" || sent.CreatedAt.IsZero() {
		warn(errChat("the hub's answer to a chat post has no id, sender or time; not recorded"))
		return
	}
	if err := ledger(chatledger.Entry{
		Recorded:     time.Now().UTC(),
		Login:        login,
		Tier:         tier,
		Conversation: key,
		AgentID:      agentID,
		MessageID:    sent.ID,
		Sender:       sent.Sender,
		CreatedAt:    chatledger.FormatTime(sent.CreatedAt),
		Text:         sent.Content,
	}); err != nil {
		warn(err)
	}
}

type errChat string

func (e errChat) Error() string { return "chat ledger: " + string(e) }
