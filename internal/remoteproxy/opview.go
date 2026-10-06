package remoteproxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

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
// the bare route, or the login and agent of a messages route. Splitting the
// escaped form keeps a "/" inside a login (legal in allowed_users) in its
// segment.
func opViewTarget(escaped string) (login, name string, list, ok bool) {
	if escaped == chatContactsPath {
		return "", "", true, true
	}
	rest, found := strings.CutPrefix(escaped, chatContactsPath+"/")
	if !found {
		return "", "", false, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[1] != "agents" || parts[3] != "messages" {
		return "", "", false, false
	}
	l, err1 := url.PathUnescape(parts[0])
	n, err2 := url.PathUnescape(parts[2])
	if err1 != nil || err2 != nil || l == "" || !agentNameRE.MatchString(n) {
		return "", "", false, false
	}
	return l, n, false, true
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
	login, name, list, ok := opViewTarget(r.URL.EscapedPath())
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
	g.serveContactHistory(w, r, line, login, name)
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

// serveContactHistory is Task 3's; until then the route is not found.
func (g *gate) serveContactHistory(w http.ResponseWriter, r *http.Request, line *AuditLine, login, name string) {
	g.refuseView(w, r, line, http.StatusNotFound, "not-found")
}
