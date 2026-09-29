package remoteproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The contact fence.
//
// A contact-tier login (config.TierContact) may only chat, and only with the
// agents its allowed_users entry lists. The hub cannot enforce the second
// half: scion binds roles per project, not per agent, and its DM checks look
// only at the USER half of a conversation key. The contact's hub role holds
// agent.message alone (lever-remote-contact), and this fence is what limits
// it to its agents and to the chat.
//
// The fence is an allow-list. Everything a contact's request may do is named
// below; everything else is refused before the hub sees it:
//
//   - static web assets, and the SPA shell for the chat page of one of its
//     own conversations; every other page gets lever's landing page, which
//     links those conversations;
//   - its own identity and the web UI's public settings;
//   - its DM conversations, dm:agent:<allowed agent>:user:<its own id>, and
//     only their messages, read, typing, mute and pin routes;
//   - a new message with no "@word" (scion routes a leading @mention to any
//     agent in the project, and fans out later ones), no reply_to_id and no
//     attachments;
//   - the event stream, with its subjects replaced by the contact's own chat
//     and notification subjects;
//   - fixed empty answers for the lists the web UI loads at start (projects,
//     spaces, agents, users) and for presence.
//
// A refusal is audited as deny-contact.

// DecisionDenyContact is the audit decision for a contact request the fence
// refused.
const DecisionDenyContact Decision = "deny-contact"

// contactCacheTTL bounds how long the fence trusts a resolved agent id or
// user id. An agent that is purged and started again gets a new id; the
// fence sees it within this time.
const contactCacheTTL = 30 * time.Second

// maxContactMessage bounds the message body the fence reads to check it.
const maxContactMessage = 1 << 20

// contactFence holds what the fence resolves from the hub.
type contactFence struct {
	resolve func(ctx context.Context) (map[string]string, error) // agent name → hub id
	whoAmI  func(ctx context.Context, cookie string) (string, error)

	mu    sync.Mutex
	ids   map[string]string // name → hub id
	idsAt time.Time
	users map[string]contactUser // login → hub user id
	nowFn func() time.Time
}

type contactUser struct {
	id string
	at time.Time
}

func (f *contactFence) now() time.Time {
	if f.nowFn != nil {
		return f.nowFn()
	}
	return time.Now()
}

// agentIDs resolves names to hub ids (from the cache when it is fresh) and
// returns id → name for the names that resolved.
func (f *contactFence) agentIDs(ctx context.Context, names []string) (map[string]string, error) {
	f.mu.Lock()
	fresh := f.ids != nil && f.now().Sub(f.idsAt) < contactCacheTTL
	ids := f.ids
	f.mu.Unlock()
	if !fresh {
		got, err := f.resolve(ctx)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.ids, f.idsAt, ids = got, f.now(), got
		f.mu.Unlock()
	}
	out := map[string]string{}
	for _, n := range names {
		if id := ids[n]; id != "" {
			out[id] = n
		}
	}
	return out, nil
}

// userID is the contact's hub user id, asked of the hub with its own session.
func (f *contactFence) userID(ctx context.Context, login, cookie string) (string, error) {
	f.mu.Lock()
	u, ok := f.users[login]
	f.mu.Unlock()
	if ok && f.now().Sub(u.at) < contactCacheTTL {
		return u.id, nil
	}
	id, err := f.whoAmI(ctx, cookie)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	if f.users == nil {
		f.users = map[string]contactUser{}
	}
	f.users[login] = contactUser{id: id, at: f.now()}
	f.mu.Unlock()
	return id, nil
}

// contactScope is what one request's checks need: the contact's hub user id
// and the hub ids of its agents, by name.
type contactScope struct {
	userID string
	agents map[string]string // hub id → name
}

func (s contactScope) key(agentID string) string { return "dm:agent:" + agentID + ":user:" + s.userID }

// allows reports whether key is one of the contact's own DM conversations.
func (s contactScope) allows(key string) bool {
	parts := strings.Split(key, ":")
	if len(parts) != 5 || parts[0] != "dm" || parts[1] != "agent" || parts[3] != "user" {
		return false
	}
	_, ok := s.agents[parts[2]]
	return ok && parts[4] == s.userID && s.userID != ""
}

// contactCanned is the fixed answer for a list the web UI loads at start.
var contactCanned = map[string]string{
	"/api/v1/projects":    `{"projects":[],"totalCount":0}`,
	"/api/v1/chat/spaces": `{"spaces":[]}`,
	"/api/v1/agents":      `{"agents":[],"totalCount":0}`,
	"/api/v1/users":       `{"users":[],"totalCount":0}`,
}

// contactForwardGET are the other GET routes a contact may use as they are.
var contactForwardGET = []string{
	"/api/v1/auth/me", "/auth/me", "/api/v1/auth/admin-status", "/api/v1/settings/public",
	"/api/v1/system/status", "/api/v1/chat/user-prefs",
}

// fenceContact decides one request from a contact. It returns the request to
// forward (possibly rewritten), or nil when it answered the request itself.
func (g *gate) fenceContact(w http.ResponseWriter, r *http.Request, line *AuditLine, login string, names []string, cookie *string) *http.Request {
	deny := func(msg string) *http.Request {
		g.deny(w, line, http.StatusForbidden, DecisionDenyContact, msg)
		return nil
	}
	p, m := r.URL.Path, r.Method
	if (m == http.MethodGet || m == http.MethodHead) && isStaticAsset(p) {
		return r
	}
	f := g.contacts
	if f == nil {
		return deny("contact access is not configured")
	}
	uid, err := f.userID(r.Context(), login, *cookie)
	if errors.Is(err, errSessionUnknown) {
		// The hub no longer knows this session (it restarted, or the
		// session lapsed): replace it once, as forward does for a GET.
		g.cfg.Session.Invalidate(login, *cookie)
		if fresh, cerr := g.cfg.Session.Cookie(r.Context(), login); cerr == nil {
			*cookie = fresh
			uid, err = f.userID(r.Context(), login, fresh)
		}
	}
	if err != nil || uid == "" {
		g.deny(w, line, http.StatusBadGateway, DecisionDenyContact, "cannot resolve your hub user")
		return nil
	}
	ids, err := f.agentIDs(r.Context(), names)
	if err != nil {
		g.deny(w, line, http.StatusBadGateway, DecisionDenyContact, "cannot resolve your agents")
		return nil
	}
	scope := contactScope{userID: uid, agents: ids}

	switch {
	case strings.HasPrefix(p, "/api/v1/chat/conversations/"):
		return g.fenceConversation(w, r, line, scope, deny)
	case p == "/events" && m == http.MethodGet:
		q := r.URL.Query()
		q["sub"] = []string{"user." + uid + ".chat.>", "user." + uid + ".notification"}
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = q.Encode()
		return r2
	case m == http.MethodGet && contactCanned[p] != "":
		g.answerContact(w, line, "application/json", contactCanned[p])
		return nil
	case m == http.MethodPost && p == "/api/v1/chat/presence":
		g.answerContact(w, line, "application/json", `{}`)
		return nil
	case (m == http.MethodGet || m == http.MethodHead) && p == "/api/v1/chat/dms":
		// The hub lists every DM the user is in, with a message preview;
		// the answer is cut down to the contact's own conversations.
		return r.WithContext(context.WithValue(r.Context(), keepDMKey{}, scope.allows))
	case (m == http.MethodGet || m == http.MethodHead) && slices.Contains(contactForwardGET, p):
		return r
	case (m == http.MethodPut && p == "/api/v1/chat/user-prefs") || (m == http.MethodPost && p == "/auth/logout"):
		return r
	case (m == http.MethodGet || m == http.MethodHead) && !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/auth/"):
		// A page. The chat page of one of its own conversations gets the
		// SPA shell; every other page gets the landing page, never the
		// hub's (the hub renders some pages with server-side data).
		if key, ok := strings.CutPrefix(p, "/chat/dm/"); ok && scope.allows(key) {
			return r
		}
		g.answerContact(w, line, "text/html; charset=utf-8", contactLanding(scope))
		return nil
	}
	return deny("a contact may only chat with its agents")
}

// fenceConversation checks a /api/v1/chat/conversations/{key}/... request.
func (g *gate) fenceConversation(w http.ResponseWriter, r *http.Request, line *AuditLine, scope contactScope, deny func(string) *http.Request) *http.Request {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/conversations/")
	key, sub, _ := strings.Cut(rest, "/")
	if !scope.allows(key) {
		return deny("not one of your conversations")
	}
	m := r.Method
	switch {
	case sub == "messages" && m == http.MethodGet:
		return r
	case sub == "messages" && m == http.MethodPost:
		return g.checkContactMessage(w, r, line, deny)
	case sub == "read" && (m == http.MethodGet || m == http.MethodPost):
		return r
	case sub == "typing" && m == http.MethodPost:
		return r
	case (sub == "mute" || sub == "pin") && m == http.MethodPut:
		return r
	}
	return deny("a contact may not use this conversation route")
}

// checkContactMessage refuses a new message that could reach another agent:
// an @word (scion's mention routing), a reply_to_id, or attachments.
func (g *gate) checkContactMessage(w http.ResponseWriter, r *http.Request, line *AuditLine, deny func(string) *http.Request) *http.Request {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxContactMessage+1))
	_ = r.Body.Close()
	if err != nil || len(body) > maxContactMessage {
		return deny("message too large or unreadable")
	}
	// Only content and idempotency_key: every other field the hub reads
	// changes routing or what the agent sees beside the verified text
	// (attachments, reply_to_id, metadata such as RE-to, which the agent's
	// envelope shows as reply_context).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return deny("message is not valid JSON")
	}
	for k := range fields {
		if k != "content" && k != "idempotency_key" {
			return deny("a contact's message may carry only content (field " + strconv.Quote(k) + " refused)")
		}
	}
	var msg struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return deny("message is not valid JSON")
	}
	// The text itself is not filtered: a contact may write anything,
	// including what looks like a lever marker. Who wrote a message is never
	// decided from its text. The proxy records this post in the chat ledger
	// as the contact's, and an agent's message_verify answers "web, tier
	// contact" with these words, whatever they say. Only a mention is
	// refused, because the hub routes it to another agent.
	if hasMention(msg.Content) {
		return deny(`a contact's message may not contain a word starting with "@" (it would route to another agent); write "at" or leave it out`)
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	return r2
}

// hasMention reports whether any whitespace-separated word starts with "@",
// the tokens scion's mention extraction looks at (pkg/messages/mentions.go).
func hasMention(s string) bool {
	for _, w := range strings.FieldsFunc(s, unicode.IsSpace) {
		if strings.HasPrefix(w, "@") || strings.HasPrefix(w, "＠") {
			return true
		}
	}
	return false
}

// contactRootFiles are the web UI's own files at the root (scion
// web/public). Any other root path would get the SPA shell.
var contactRootFiles = []string{"/favicon.svg", "/scion-notification-icon.png"}

// isStaticAsset reports whether p is one of the web UI's own static files,
// which carry no data.
func isStaticAsset(p string) bool {
	return strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/shoelace/") || slices.Contains(contactRootFiles, p)
}

// answerContact writes a fixed answer and audits it as allowed.
func (g *gate) answerContact(w http.ResponseWriter, line *AuditLine, contentType, body string) {
	line.Decision, line.Status = DecisionAllow, http.StatusOK
	g.audit(*line)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, body)
}

// contactLanding is lever's page for a contact: links to its conversations.
// No script; the hub's own pages are never shown to a contact.
func contactLanding(s contactScope) string {
	ids := make([]string, 0, len(s.agents))
	for id := range s.agents {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return strings.Compare(s.agents[a], s.agents[b]) })
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>Chat</title><style>body{font:16px system-ui,sans-serif;margin:2rem auto;max-width:36rem;padding:0 1rem}` +
		`a{display:block;padding:.75rem 0}</style></head><body><h1>Chat</h1>`)
	if len(ids) == 0 {
		b.WriteString("<p>No agent is available to you right now. Try again later.</p>")
	}
	for _, id := range ids {
		fmt.Fprintf(&b, `<a href="/chat/dm/%s">%s</a>`, url.PathEscape(s.key(id)), html.EscapeString(s.agents[id]))
	}
	b.WriteString("</body></html>")
	return b.String()
}

// errNoUserID means the hub's /auth/me answer carried no id.
var errNoUserID = errors.New("the hub named no user id")

// errSessionUnknown means the hub answered 401 to the login's session.
var errSessionUnknown = errors.New("the hub does not know this session")

// hubWhoAmI asks the hub, with a login's own session, for its user id.
func hubWhoAmI(cfg Config) func(ctx context.Context, cookie string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	if cfg.DialContext != nil {
		client.Transport = jailTransport(cfg.DialContext)
	}
	return func(ctx context.Context, cookie string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Target.JoinPath("/api/v1/auth/me").String(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Cookie", sessionCookieName+"="+cookie)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return "", errSessionUnknown
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("hub /api/v1/auth/me: HTTP %d", resp.StatusCode)
		}
		var me struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&me); err != nil {
			return "", err
		}
		if me.ID == "" {
			return "", errNoUserID
		}
		return me.ID, nil
	}
}

// HubDoer is a hubapi.Doer over the proxy's own route to the hub, with a
// bearer token: the remote PAT, which can also attach and message, so the
// fence only ever sends GET with it (Do has no body). The contact fence
// resolves agent names with it.
type HubDoer struct {
	Target      *url.URL
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Token       func() (string, error)
	client      *http.Client
	once        sync.Once
}

func (d *HubDoer) Do(ctx context.Context, method, p string) (int, []byte, error) {
	d.once.Do(func() {
		d.client = &http.Client{Timeout: 15 * time.Second}
		if d.DialContext != nil {
			d.client.Transport = jailTransport(d.DialContext)
		}
	})
	u, err := d.Target.Parse(p)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	tok, err := d.Token()
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

type keepDMKey struct{}

// contactKeepDM is the DM filter fenceContact attached to r, if any.
func contactKeepDM(r *http.Request) func(string) bool {
	f, _ := r.Context().Value(keepDMKey{}).(func(string) bool)
	return f
}

// filterDMList keeps only the DMs keep allows in a /api/v1/chat/dms answer.
// An answer it cannot read is replaced by an empty list: failing closed.
func filterDMList(resp *http.Response, keep func(string) bool) {
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()
	out := []byte(`{"dms":[]}`)
	var doc map[string]json.RawMessage
	if err == nil && resp.Header.Get("Content-Encoding") == "" && json.Unmarshal(body, &doc) == nil {
		var dms []map[string]json.RawMessage
		if json.Unmarshal(doc["dms"], &dms) == nil {
			kept := []map[string]json.RawMessage{}
			for _, d := range dms {
				var key string
				if json.Unmarshal(d["conversationKey"], &key) == nil && keep(key) {
					kept = append(kept, d)
				}
			}
			doc["dms"], _ = json.Marshal(kept)
			if b, err := json.Marshal(doc); err == nil {
				out = b
			}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Content-Encoding")
}
