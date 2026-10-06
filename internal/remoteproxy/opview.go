package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The operator's read-only view of contact conversations (spec 5).
//
// GET /lever/api/contacts lists every contact login with the agents it may
// message; GET /lever/api/contacts/<login>/agents/<name>/messages reads one
// page of that contact's DM with that agent. The hub lets a person read only
// the DMs whose key names them (scion isDMParticipant), so the proxy reads
// with the CONTACT's own hub session, server-side, and answers the operator
// rows reduced to text. The rules:
//
//   - operator logins only: a contact gets the fence's own refusal, whatever
//     the method, and the routes answer GET and HEAD only;
//   - <login> and <name> must be config entries (a contact and one of its
//     agents:), matched exactly; the conversation key is built from the hub's
//     agent record and the contact's bound user id, never from the request;
//   - the contact's session makes one request kind: GET of that one history
//     route (hubGetBody). Nothing is posted, marked read or typed;
//   - a contact `lever apply` has not bound to a hub user is answered
//     "not-signed-in", and no session is asked for: a login would create it.

const chatContactsPath = "/lever/api/contacts"

// Operator view audit decisions.
const (
	DecisionOperatorView     Decision = "operator-view"
	DecisionDenyOperatorView Decision = "deny-operator-view"
)

type contactAgent struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	State string `json:"state"`
}

type contactEntry struct {
	Login    string         `json:"login"`
	SignedIn bool           `json:"signedIn"`
	Agents   []contactAgent `json:"agents"`
}

type contactsAnswer struct {
	Contacts []contactEntry `json:"contacts"`
}

// opViewTarget splits an ESCAPED path under /lever/api/contacts: list for
// the bare route, or the login and agent of a messages route (files for the
// files route, remote.files). Splitting the
// escaped form keeps a "/" inside a login (legal in allowed_users) in its
// segment; the proxy's path check (pathcheck.go) refuses such a path before
// it gets here, so a login holding "/" cannot be opened.
func opViewTarget(escaped string) (login, name string, list, files, ok bool) {
	if escaped == chatContactsPath {
		return "", "", true, false, true
	}
	rest, found := strings.CutPrefix(escaped, chatContactsPath+"/")
	if !found {
		return "", "", false, false, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[1] != "agents" || parts[3] != "messages" && parts[3] != "files" {
		return "", "", false, false, false
	}
	l, err1 := url.PathUnescape(parts[0])
	n, err2 := url.PathUnescape(parts[2])
	if err1 != nil || err2 != nil || l == "" || !agentNameRE.MatchString(n) {
		return "", "", false, false, false
	}
	return l, n, false, parts[3] == "files", true
}

// contactLogins is every contact login, in allowed_users order.
func (g *gate) contactLogins() []string {
	var out []string
	for _, l := range g.cfg.AllowedUsers {
		if _, ok := g.cfg.Contacts[l]; ok && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// contactUser is the hub user id lever bound login to, when it bound one
// that can be a key part.
func (g *gate) contactUser(login string) (string, bool) {
	if g.cfg.ContactUser == nil {
		return "", false
	}
	uid, ok := g.cfg.ContactUser(login)
	return uid, ok && validHubID(uid)
}

// serveOperatorView answers both routes for v (the requesting login).
func (g *gate) serveOperatorView(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer) {
	if v.tier != chatledger.TierOperator {
		g.deny(w, line, http.StatusForbidden, DecisionDenyContact, "a contact may only chat with its agents")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		g.refuseView(w, r, line, http.StatusMethodNotAllowed, "method")
		return
	}
	login, name, list, files, ok := opViewTarget(r.URL.EscapedPath())
	if !ok {
		g.refuseView(w, r, line, http.StatusNotFound, "not-found")
		return
	}
	if list {
		g.serveContactList(w, r, line)
		return
	}
	agents, isContact := g.cfg.Contacts[login]
	if !isContact || !slices.Contains(agents, name) {
		g.refuseView(w, r, line, http.StatusNotFound, "not-found")
		return
	}
	if files {
		g.serveContactFiles(w, r, line, login, name)
		return
	}
	g.serveContactHistory(w, r, line, login, name)
}

// serveContactFiles answers a contact's file exchange with one of its
// agents (remote.files), from the files ledger alone: the same rows the
// contact's own Files panel shows ("sent" = the contact's upload). Each row
// links to the normal download route, where mayDownload lets an operator
// fetch any record of an agent it may message.
func (g *gate) serveContactFiles(w http.ResponseWriter, r *http.Request, line *AuditLine, login, name string) {
	line.Contact, line.Agent = truncateAudit(login), truncateAudit(name)
	if g.files == nil {
		g.refuseView(w, r, line, http.StatusNotFound, "not-found")
		return
	}
	files, err := g.filesFor(name, login)
	if err != nil {
		line.Error = err.Error()
		g.refuseView(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	n := len(files)
	line.Count = &n
	g.answerViewJSON(w, r, line, DecisionOperatorView, http.StatusOK, map[string]any{"files": files})
}

// serveContactList answers the contacts and their message agents, from
// config, the hub's records and the labels: no contact's session.
func (g *gate) serveContactList(w http.ResponseWriter, r *http.Request, line *AuditLine) {
	recs, err := g.records(r.Context())
	labels := g.labels()
	ans := contactsAnswer{Contacts: []contactEntry{}}
	for _, login := range g.contactLogins() {
		e := contactEntry{Login: login, Agents: []contactAgent{}}
		_, e.SignedIn = g.contactUser(login)
		for _, n := range managerFirst(g.cfg.ChatAgent, g.cfg.Contacts[login]) {
			rec, found := recs[n]
			down := err != nil
			if found && !validHubID(rec.ID) {
				rec, down = AgentRecord{}, true
			}
			state, _ := agentState(rec, found, down)
			e.Agents = append(e.Agents, contactAgent{Name: n, Label: labels[n], State: state})
		}
		ans.Contacts = append(ans.Contacts, e)
	}
	n := len(ans.Contacts)
	line.Count = &n
	g.answerViewJSON(w, r, line, DecisionOperatorView, http.StatusOK, ans)
}

// refuseView answers {"error": word} with status and audits the word.
func (g *gate) refuseView(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerViewJSON(w, r, line, DecisionDenyOperatorView, status, map[string]string{"error": word})
}

// answerViewJSON writes one JSON answer with the chat page's headers, as an
// inert document (like serveAgents).
func (g *gate) answerViewJSON(w http.ResponseWriter, r *http.Request, line *AuditLine, decision Decision, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		b, status, decision = []byte(`{"error":"unavailable"}`), http.StatusBadGateway, DecisionDenyOperatorView
	}
	g.answerChat(w, line, decision, status, func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, append(b, '\n'), r)
}

// Hub history paging (pkg/hub/handlers_chat_v2.go handleConversationHistory).
const (
	viewPage      = 50
	viewMaxPage   = 200
	viewMaxCursor = 512
)

type viewRow struct {
	ID             string `json:"id"`
	From           string `json:"from"` // "contact" | "agent" | "system"
	Text           string `json:"text"`
	CreatedAt      string `json:"createdAt"`
	ShownToContact bool   `json:"shownToContact"`
	// Pending marks an agent row a record would bind but no contact read
	// has bound yet: the contact has not been shown it, and will be when
	// its read binds this row (the record may bind another of the same
	// text instead, depending on the page the contact reads).
	Pending bool `json:"pending,omitempty"`
}

type viewAnswer struct {
	Contact    string    `json:"contact"`
	Agent      string    `json:"agent"`
	Messages   []viewRow `json:"messages"`
	NextCursor string    `json:"nextCursor,omitempty"`
	// Matched is false when agent messages are on and the broker did not
	// answer: no agent row is then shown to the contact, and the page says
	// the contact's view is not known.
	Matched bool `json:"matched"`
}

// historyQuery is the hub query for a page: limit (default viewPage, 1 to
// viewMaxPage) and an opaque cursor of printable ASCII. Every other key is
// dropped, so nothing else of the request reaches the hub.
func historyQuery(q url.Values) (string, bool) {
	out := url.Values{"limit": {strconv.Itoa(viewPage)}}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > viewMaxPage {
			return "", false
		}
		out.Set("limit", strconv.Itoa(n))
	}
	if c := q.Get("cursor"); c != "" {
		if len(c) > viewMaxCursor {
			return "", false
		}
		for i := 0; i < len(c); i++ {
			if c[i] < 0x21 || c[i] > 0x7e {
				return "", false
			}
		}
		out.Set("cursor", c)
	}
	return out.Encode(), true
}

// rowFrom names who wrote a row: the contact (its own rows), the hub (a
// line no agent can write, agentRow) or the agent (everything else).
func rowFrom(m historyRow, uid, agentID string) string {
	if agentRow(m, uid, agentID) {
		return "agent"
	}
	if m.SenderID == uid && uid != "" && strings.HasPrefix(m.Sender, "user:") {
		return "contact"
	}
	return "system"
}

// serveContactHistory reads one page of login's DM with the agent name,
// with login's own hub session.
func (g *gate) serveContactHistory(w http.ResponseWriter, r *http.Request, line *AuditLine, login, name string) {
	line.Contact, line.Agent = truncateAudit(login), truncateAudit(name)
	ctx := r.Context()
	uid, bound := g.contactUser(login)
	if !bound {
		g.refuseView(w, r, line, http.StatusConflict, "not-signed-in")
		return
	}
	query, ok := historyQuery(r.URL.Query())
	if !ok {
		g.refuseView(w, r, line, http.StatusBadRequest, "bad-query")
		return
	}
	recs, err := g.records(ctx)
	if err != nil {
		g.refuseView(w, r, line, http.StatusBadGateway, "unavailable")
		return
	}
	rec, found := recs[name]
	if !found {
		g.refuseView(w, r, line, http.StatusConflict, "no-record")
		return
	}
	if !validHubID(rec.ID) {
		g.refuseView(w, r, line, http.StatusBadGateway, "unavailable")
		return
	}
	key := "dm:agent:" + rec.ID + ":user:" + uid
	body, cookie, err := g.readAsContact(ctx, login, "/api/v1/chat/conversations/"+url.PathEscape(key)+"/messages?"+query)
	if err != nil {
		if errors.Is(err, errViewHub) && g.staleBinding(ctx, cookie, uid) {
			// The hub refuses a key that does not name the session's user:
			// apply bound the contact to a hub user it no longer is.
			line.Reason = "stale-binding"
			g.answerViewJSON(w, r, line, DecisionDenyOperatorView, http.StatusConflict,
				map[string]string{"error": "not-signed-in", "hint": "run lever apply"})
			return
		}
		g.refuseView(w, r, line, http.StatusBadGateway, "unavailable")
		return
	}
	ans, ok := g.viewRows(ctx, login, name, rec.ID, uid, body)
	if !ok {
		g.refuseView(w, r, line, http.StatusBadGateway, "unavailable")
		return
	}
	ans.Contact, ans.Agent = login, name
	n := len(ans.Messages)
	line.Count = &n
	g.answerViewJSON(w, r, line, DecisionOperatorView, http.StatusOK, ans)
}

var errViewHub = errors.New("the hub did not answer the history read")

// readAsContact GETs path with login's session; a session the hub no longer
// knows (401, or a redirect to its login page: any 3xx, never followed) is
// replaced once. It returns the session it read with last; errViewHub
// means the hub answered that session with something other than a 200.
func (g *gate) readAsContact(ctx context.Context, login, path string) ([]byte, string, error) {
	cookie, err := g.cfg.Session.Cookie(ctx, login)
	if err != nil {
		return nil, "", err
	}
	status, body, err := g.chat.hubBody(ctx, cookie, path)
	if err == nil && (status == http.StatusUnauthorized || status >= 300 && status < 400) {
		g.cfg.Session.Invalidate(login, cookie)
		if cookie, err = g.cfg.Session.Cookie(ctx, login); err != nil {
			return nil, "", err
		}
		status, body, err = g.chat.hubBody(ctx, cookie, path)
	}
	if err != nil {
		return nil, cookie, err
	}
	if status != http.StatusOK {
		return nil, cookie, errViewHub
	}
	return body, cookie, nil
}

// staleBinding reports whether the contact's session belongs to a hub user
// other than uid, the one apply bound: the hub then refuses the history of
// the bound key. It asks only GET /api/v1/auth/me (hubWhoAmI: no redirect
// followed, a bounded answer), and only after a failed read. A session the
// hub cannot name is not stale: the read is then "unavailable".
func (g *gate) staleBinding(ctx context.Context, cookie, uid string) bool {
	if cookie == "" {
		return false
	}
	id, err := g.chat.whoAmI(ctx, cookie)
	return err == nil && id != uid
}

// viewRows maps a hub history answer to the operator's rows. Each agent
// row is marked with what the contact is shown (contactShown, the rule
// filterHistory applies); with agent messages off the contact sees every
// row. The broker is asked with a peek: the operator's read binds no
// record, so which of two same-text messages a record shows is decided by
// the contact's own reads alone, and the peek tells a bound row (shown)
// from one a record would bind (pending). The marks are per page. An
// unreadable row reads as an agent row with no text, as in filterHistory.
func (g *gate) viewRows(ctx context.Context, contact, agent, agentID, uid string, body []byte) (viewAnswer, bool) {
	var doc struct {
		Messages   []json.RawMessage `json:"messages"`
		NextCursor string            `json:"nextCursor"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return viewAnswer{}, false
	}
	rows := make([]historyRow, len(doc.Messages))
	for i, raw := range doc.Messages {
		if json.Unmarshal(raw, &rows[i]) != nil {
			rows[i] = historyRow{Sender: "agent:?"}
		}
	}
	ans := viewAnswer{Messages: []viewRow{}, NextCursor: doc.NextCursor, Matched: true}
	var shown map[string]bool // nil: every row is shown
	pending := map[string]bool{}
	if g.cfg.MatchAgentMessages != nil {
		var peek func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error)
		if g.cfg.PeekAgentMessages != nil {
			peek = func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error) {
				keep, p, err := g.cfg.PeekAgentMessages(ctx, contact, agent, msgs)
				pending = p
				return keep, err
			}
		}
		keep, err := askAgentRows(ctx, peek, contact, agent, agentID, uid, rows)
		if err != nil {
			keep, pending, ans.Matched = map[string]bool{}, map[string]bool{}, false
		}
		shown = contactShown(rows, keep, uid, agentID)
	}
	for _, m := range rows {
		// Only a row the contact's rule keeps can be pending: askAgentRows
		// already dropped every id the question did not name.
		p := shown != nil && shown[m.ID] && agentRow(m, uid, agentID) && pending[m.ID]
		ans.Messages = append(ans.Messages, viewRow{ID: m.ID, From: rowFrom(m, uid, agentID), Text: m.Msg,
			CreatedAt: m.CreatedAt, ShownToContact: (shown == nil || shown[m.ID]) && !p, Pending: p})
	}
	return ans, true
}

var errHistoryTooBig = errors.New("the hub's history answer is too big")

// hubGetBody reads a hub route with a login's own session: GET and nothing
// else, no redirect followed (a lapsed session answers one), at most max
// bytes of a 200 answer.
func hubGetBody(cfg Config, max int64) func(ctx context.Context, cookie, path string) (int, []byte, error) {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: noRedirect}
	if cfg.DialContext != nil {
		client.Transport = jailTransport(cfg.DialContext)
	}
	return func(ctx context.Context, cookie, path string) (int, []byte, error) {
		ref, err := url.Parse(path)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Target.ResolveReference(ref).String(), nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Cookie", sessionCookieName+"="+cookie)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil, nil
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err == nil && int64(len(b)) > max {
			err = errHistoryTooBig
		}
		return resp.StatusCode, b, err
	}
}
