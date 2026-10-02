package remoteproxy

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The chat page.
//
// With Config.ChatAgent set (remote.landing: chat), the proxy serves a small
// page of its own under /lever/ and sends "/" there: one conversation, the
// operator's DM with the manager, plus links to the manager's terminal and to
// the hub's full web UI. It is an alternative front for a route that already
// exists, not a new route to the agent:
//
//   - The page reads and sends through the hub's own chat routes, forwarded
//     by this proxy like any other request. A message it posts is therefore
//     recorded by recordChat exactly as one posted from the hub's web UI, and
//     verifies the same way (package chatledger). Nothing here signs,
//     records or delivers a message.
//   - Everything below is answered only for an operator-tier login that
//     passed the gate. A contact never gets here: ServeHTTP hands its
//     requests to the contact fence instead, which answers every page with
//     its own landing page.
//   - What lever serves is fixed: the files embedded in the binary, and one
//     JSON answer (the bootstrap) built from the verified login and from two
//     ids the hub names, each checked before it is used. No agent-written
//     byte is served from here. The page writes what it reads from the hub as
//     text only (see chatui/chat.js), and its CSP allows no inline script.
//
// The whole /lever/ prefix is lever's while the page is on: an unknown path
// under it is a 404, never forwarded, so a route added here later cannot
// shadow one the hub answers.

//go:embed chatui/chat.html chatui/chat.css chatui/chat.js chatui/chatcore.js
var chatUI embed.FS

// DecisionChatUnavailable is the audit decision for a chat page request the
// proxy could not answer (the hub named no usable identity or agent).
const DecisionChatUnavailable Decision = "chat-unavailable"

const (
	chatPrefix        = "/lever/"
	chatPagePath      = "/lever/chat"
	chatBootstrapPath = "/lever/api/chat"
	// chatConsolePath is where the page's link to the hub's web UI goes: its
	// agent list, since "/" now leads back to the chat page.
	chatConsolePath = "/agents"
)

// chatCSP lets the page load lever's own script and style and talk to its
// own origin, and nothing else: no inline script or style, no frame, no form
// target, no other origin.
const chatCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// chatFile is one embedded file, read once.
type chatFile struct {
	contentType string
	body        []byte
	etag        string
}

// chatPage serves the page for one manager agent.
type chatPage struct {
	agent   string // the manager's agent name (Config.ChatAgent)
	files   map[string]chatFile
	resolve func(ctx context.Context) (map[string]string, error) // agent name → hub id
	whoAmI  func(ctx context.Context, cookie string) (string, error)
}

// newChatPage loads the embedded files. A file that is missing is a build
// fault, so it panics rather than serve a page with a hole in it.
func newChatPage(cfg Config) *chatPage {
	p := &chatPage{agent: cfg.ChatAgent, resolve: cfg.ResolveAgents, whoAmI: hubWhoAmI(cfg), files: map[string]chatFile{}}
	for route, f := range map[string]struct{ name, contentType string }{
		chatPagePath:         {"chatui/chat.html", "text/html; charset=utf-8"},
		"/lever/chat.css":    {"chatui/chat.css", "text/css; charset=utf-8"},
		"/lever/chat.js":     {"chatui/chat.js", "text/javascript; charset=utf-8"},
		"/lever/chatcore.js": {"chatui/chatcore.js", "text/javascript; charset=utf-8"},
	} {
		body, err := chatUI.ReadFile(f.name)
		if err != nil {
			panic("remoteproxy: embedded chat page file: " + err.Error())
		}
		sum := sha256.Sum256(body)
		p.files[route] = chatFile{contentType: f.contentType, body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	}
	return p
}

// chatBootstrap is what the page needs to find its conversation.
type chatBootstrap struct {
	Login  string       `json:"login"`
	UserID string       `json:"userId"`
	Agent  chatAgentRef `json:"agent"`
	// Conversation is the DM key, and Terminal the hub web UI's terminal
	// page for the agent. Both are empty while the agent has no hub record.
	Conversation string `json:"conversation,omitempty"`
	Terminal     string `json:"terminal,omitempty"`
	Console      string `json:"console"`
}

type chatAgentRef struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// validHubID reports whether an id the hub named is safe to put in a URL
// path and a conversation key: the page builds both from it, on the
// operator's origin. Hub ids are UUIDs; anything with a separator in it is
// refused rather than escaped.
func validHubID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// serveChatPage answers a request the chat page owns and reports true, or
// reports false for a request that goes to the hub as before. operator is
// the verified operator-tier login and cookie its hub session; the caller
// has already run authorize and ruled out a contact.
func (g *gate) serveChatPage(w http.ResponseWriter, r *http.Request, line *AuditLine, operator, cookie string) bool {
	if g.chat == nil || operator == "" {
		// No verified login (allowed_users empty): nothing the page sent
		// would verify, so there is no page.
		return false
	}
	p := r.URL.Path
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if p == "/" {
		if !read {
			return false
		}
		// A fixed target: nothing of the request goes into Location.
		g.answerChat(w, line, http.StatusFound, func() { w.Header().Set("Location", chatPagePath) }, nil, r)
		return true
	}
	if p != strings.TrimSuffix(chatPrefix, "/") && !strings.HasPrefix(p, chatPrefix) {
		return false
	}
	if !read {
		w.Header().Set("Allow", "GET, HEAD")
		g.answerChat(w, line, http.StatusMethodNotAllowed, nil, []byte("method not allowed\n"), r)
		return true
	}
	if p == chatBootstrapPath {
		g.serveChatBootstrap(w, r, line, operator, cookie)
		return true
	}
	f, ok := g.chat.files[p]
	if !ok {
		g.answerChat(w, line, http.StatusNotFound, nil, []byte("not found\n"), r)
		return true
	}
	hdr := func() {
		w.Header().Set("Content-Type", f.contentType)
		w.Header().Set("ETag", f.etag)
		// Revalidate on every load: the files change with the lever binary.
		w.Header().Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == f.etag {
		g.answerChat(w, line, http.StatusNotModified, hdr, nil, r)
		return true
	}
	g.answerChat(w, line, http.StatusOK, hdr, f.body, r)
	return true
}

// answerChat writes one of the chat page's own answers and audits it. Every
// answer carries the page's CSP, whatever its type: a 404 text is then as
// inert as the page is strict.
func (g *gate) answerChat(w http.ResponseWriter, line *AuditLine, status int, headers func(), body []byte, r *http.Request) {
	line.Decision, line.Status = DecisionAllow, status
	g.audit(*line)
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Security-Policy", chatCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	if headers != nil {
		headers()
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// serveChatBootstrap answers the bootstrap: who the operator is to the hub,
// and which agent and conversation the page is for.
func (g *gate) serveChatBootstrap(w http.ResponseWriter, r *http.Request, line *AuditLine, operator, cookie string) {
	c := g.chat
	unavailable := func(msg string) {
		g.deny(w, line, http.StatusBadGateway, DecisionChatUnavailable, msg)
	}
	if c.resolve == nil {
		unavailable("the chat page cannot resolve its agent")
		return
	}
	uid, err := c.whoAmI(r.Context(), cookie)
	if errors.Is(err, errSessionUnknown) {
		// The hub no longer knows this session (it restarted, or the
		// session lapsed): replace it once, as forward does for a GET.
		g.cfg.Session.Invalidate(operator, cookie)
		if fresh, cerr := g.cfg.Session.Cookie(r.Context(), operator); cerr == nil {
			uid, err = c.whoAmI(r.Context(), fresh)
		}
	}
	if err != nil || !validHubID(uid) {
		unavailable("cannot resolve your hub user")
		return
	}
	ids, err := c.resolve(r.Context())
	if err != nil {
		unavailable("cannot resolve the manager agent")
		return
	}
	out := chatBootstrap{Login: operator, UserID: uid, Agent: chatAgentRef{Name: c.agent}, Console: chatConsolePath}
	// No id is not a fault: the manager has no hub record until its first
	// start. The page says so.
	if id := ids[c.agent]; id != "" {
		if !validHubID(id) {
			unavailable("the hub named an agent id the chat page cannot use")
			return
		}
		out.Agent.ID = id
		out.Conversation = "dm:agent:" + id + ":user:" + uid
		out.Terminal = "/agents/" + id + "/terminal"
	}
	body, err := json.Marshal(out)
	if err != nil {
		unavailable("cannot encode the chat page's data")
		return
	}
	g.answerChat(w, line, http.StatusOK, func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// Like every /api/ answer the proxy forwards (sandboxAPIDocument):
		// opened as a page, this is an inert document.
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, body, r)
}
