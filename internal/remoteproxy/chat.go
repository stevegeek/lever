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
// verified login, so an agent can ask the broker whether a message took this
// path (package chatledger). Only a 201 POST into an agent DM
// (/api/v1/chat/conversations/dm:agent:<agentID>:user:<userID>/messages) is
// recorded. Topic threads, the quick-message dialog (client-chosen
// timestamp), edits, 200 idempotency replays and terminal input are not, so
// they never verify: the safe direction.

const chatConversationsPrefix = "/api/v1/chat/conversations/"

// maxChatResponse bounds the buffered hub answer, which echoes a message of
// at most 16000 characters (scion's messages.MaxMessageLength) plus metadata.
const maxChatResponse = 1 << 20

// chatDMAgent reports the conversation key and agent id when p (decoded) is
// an agent DM's send route.
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

// chatSendResponse is the part of scion's chatMessageResponse the ledger keeps.
type chatSendResponse struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Sender    string    `json:"sender"`
	CreatedAt time.Time `json:"createdAt"`
}

// recordChat appends the ledger entry for a recorded chat post from a
// verified login, handing the client the hub's answer unchanged. A recording
// failure goes to warn and never fails the request: the message is already
// delivered, and an unrecorded message only fails to verify.
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
	// Re-chain the read bytes with the rest of the stream, so an error or an
	// oversized answer never cuts what reaches the browser.
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
