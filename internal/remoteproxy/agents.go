package remoteproxy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The chat page's agent list (GET /lever/api/agents).
//
// The list is built here, per login, from what the server knows: the
// login's tier and lists from lever.yaml, the hub's agent records (read with
// the remote PAT, GET only), the labels file, and the login's own hub
// session (its user id and DM read state). An agent outside the login's
// lists never appears; a see-only agent carries no id, conversation, unread
// count or activity, so the page has nothing to open its history with. The
// page talks to the hub through the proxy for everything else, behind the
// contact fence as before.

// AgentRecord is one hub agent record as the chat page may use it. Phase
// and Activity are already reduced to scion's known words
// (scion.PhaseLabel, ActivityLabel): they are agent-reported text.
type AgentRecord struct {
	ID, Phase, Activity string
	// ContainerDown: the hub says running, the container is not live
	// (scion.RunningContainerDown).
	ContainerDown bool
}

// viewer is what one login may see on the chat page.
type viewer struct {
	login, tier  string   // tier: chatledger.TierOperator | chatledger.TierContact
	message, see []string // agent names, manager first, then config order
}

type agentEntry struct {
	Name         string `json:"name"`
	Role         string `json:"role"` // "manager" | "worker"
	Label        string `json:"label"`
	Access       string `json:"access"` // "message" | "see"
	State        string `json:"state"`
	ID           string `json:"id,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Activity     string `json:"activity,omitempty"`
	Unread       *int   `json:"unread,omitempty"`
	Terminal     string `json:"terminal,omitempty"` // operator only
}

type agentsAnswer struct {
	Login   string       `json:"login"`
	Tier    string       `json:"tier"`
	UserID  string       `json:"userId"`
	Console string       `json:"console,omitempty"` // operator only
	Agents  []agentEntry `json:"agents"`
	Files   *filesInfo   `json:"files,omitempty"` // remote.files on
}

// filesInfo is the upload limits the page checks before it sends a file
// (the proxy checks them again).
type filesInfo struct {
	MaxBytes   int64    `json:"maxBytes"`
	Extensions []string `json:"extensions"`
}

const (
	accessMessage = "message"
	accessSee     = "see"
)

// agentState maps a hub record to the page's fixed state words. Phase and
// activity are agent-reported text the caller already reduced to scion's
// known words; anything else is "unknown" (state) or left out (activity).
func agentState(rec AgentRecord, found, hubDown bool) (string, string) {
	switch {
	case hubDown:
		return "unknown", ""
	case !found:
		return "no-record", ""
	}
	switch rec.Phase {
	case "running":
		if rec.ContainerDown {
			return "error", ""
		}
		return "running", activityWord(rec.Activity)
	case "resumed", "created", "provisioning", "cloning", "starting":
		// "resumed" is scion's interim phase after a resume: the hub refuses
		// a message until the agent runs, so it reads as starting.
		return "starting", ""
	case "suspended":
		return "suspended", ""
	case "stopping", "stopped":
		return "stopped", ""
	case "error":
		return "error", ""
	}
	return "unknown", ""
}

func activityWord(a string) string {
	switch a {
	case "working", "thinking", "executing":
		return "working"
	case "waiting_for_input", "blocked":
		return "waiting"
	case "completed", "stalled":
		// stalled = no report for a while: what an agent at its prompt looks
		// like (the predecessor page reads it as idle too).
		return "idle"
	}
	return ""
}

// viewerFor is what login may see. An operator messages the manager and
// every worker; a contact messages its agents: list and sees its see: list.
// The manager, when listed, comes first.
func (g *gate) viewerFor(login string) viewer {
	mgr := g.cfg.ChatAgent
	if names, ok := g.cfg.Contacts[login]; ok {
		return viewer{login: login, tier: chatledger.TierContact,
			message: managerFirst(mgr, names), see: managerFirst(mgr, g.cfg.ContactSee[login])}
	}
	return viewer{login: login, tier: chatledger.TierOperator, message: append([]string{mgr}, g.cfg.Workers...)}
}

func managerFirst(mgr string, names []string) []string {
	out := make([]string, 0, len(names))
	if slices.Contains(names, mgr) {
		out = append(out, mgr)
	}
	for _, n := range names {
		if n != mgr && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// mayMessage reports whether v may message name.
func (v viewer) mayMessage(name string) bool { return slices.Contains(v.message, name) }

// buildAgents is the list for v: the manager first (whichever list it is
// in), then the message agents in config order, then the see agents. fresh
// is Config.ContactSession.
func buildAgents(v viewer, manager, uid string, recs map[string]AgentRecord, hubDown bool,
	labels map[string]string, fresh func(string) error) agentsAnswer {
	op := v.tier == chatledger.TierOperator
	ans := agentsAnswer{Login: v.login, Tier: v.tier, UserID: uid, Agents: []agentEntry{}}
	if op {
		ans.Console = chatConsolePath
	}
	entry := func(name, access string) agentEntry {
		e := agentEntry{Name: name, Role: "worker", Label: labels[name], Access: access}
		if name == manager {
			e.Role = "manager"
		}
		rec, found := recs[name]
		down := hubDown
		if found && !validHubID(rec.ID) {
			// A record whose id cannot be a path segment reads as unknown,
			// and no id, conversation or link is built from it.
			rec, down = AgentRecord{}, true
		}
		e.State, e.Activity = agentState(rec, found, down)
		if access == accessSee {
			e.Activity = ""
			return e
		}
		if v.tier == chatledger.TierContact && !contactFresh(e.State, name, fresh) {
			e.State, e.Activity = "not-fresh", ""
		}
		if found && rec.ID != "" && e.State != "unknown" {
			e.ID = rec.ID
			e.Conversation = "dm:agent:" + rec.ID + ":user:" + uid
			if op {
				e.Terminal = "/agents/" + rec.ID + "/terminal"
			}
		}
		return e
	}
	type row struct{ name, access string }
	var rows []row
	for _, n := range v.message {
		rows = append(rows, row{n, accessMessage})
	}
	for _, n := range v.see {
		rows = append(rows, row{n, accessSee})
	}
	// The manager first, whichever list names it.
	slices.SortStableFunc(rows, func(a, b row) int {
		return cmp.Compare(boolInt(b.name == manager), boolInt(a.name == manager))
	})
	for _, r := range rows {
		ans.Agents = append(ans.Agents, entry(r.name, r.access))
	}
	return ans
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// contactFresh reports whether a contact's message agent in state may take
// its posts as far as the session check goes. "no-record" and "unknown"
// keep their own words. A nil fresh (no Config.ContactSession) refuses every
// contact post in the fence (sessionAllows), so it reads as not fresh here.
func contactFresh(state, name string, fresh func(string) error) bool {
	if state == "no-record" || state == "unknown" {
		return true
	}
	return fresh != nil && fresh(name) == nil
}

// Unread counts.
const (
	maxUnread      = 99
	unreadPage     = 100
	maxUnreadReads = 8
	unreadBudget   = 10 * time.Second
)

type hubMessage struct {
	ID        string `json:"id"`
	SenderID  string `json:"senderId"`
	Sender    string `json:"sender"`
	Type      string `json:"type"`
	Msg       string `json:"msg"`
	CreatedAt string `json:"createdAt"`
}

// countUnread counts the agent's messages newer than lastRead in one page
// of history (sorted here, newest first, so the hub's order does not
// matter). A marker that is not in a full page means at least a page: cap.
// keep, when set, is the agent-messages filter: only the agent rows it
// keeps count (the rows the login is shown).
func countUnread(items []hubMessage, agentID, lastRead string, full bool, keep func(hubMessage) bool) int {
	at := func(m hubMessage) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, m.CreatedAt)
		return t
	}
	items = slices.Clone(items)
	slices.SortFunc(items, func(a, b hubMessage) int {
		return cmp.Or(at(b).Compare(at(a)), cmp.Compare(b.ID, a.ID))
	})
	n := 0
	for _, m := range items {
		if lastRead != "" && m.ID == lastRead {
			return min(n, maxUnread)
		}
		if m.SenderID == agentID && (keep == nil || keep(m)) {
			n++
		}
	}
	if full {
		return maxUnread
	}
	return min(n, maxUnread)
}

// hubDM is one entry of the hub's /api/v1/chat/dms answer.
type hubDM struct {
	ConversationKey   string `json:"conversationKey"`
	LastReadMessageID string `json:"lastReadMessageId"`
	HasUnread         bool   `json:"hasUnread"`
}

// addUnread sets each message agent's unread count from the login's own DM
// read state. Any fault leaves the counts out: the page shows no badge
// rather than a wrong one.
func (g *gate) addUnread(ctx context.Context, login, cookie string, ans *agentsAnswer) {
	ctx, cancel := context.WithTimeout(ctx, unreadBudget)
	defer cancel()
	c := g.chat
	var list struct {
		DMs *[]hubDM `json:"dms"`
	}
	status, err := c.hubGet(ctx, cookie, "/api/v1/chat/dms", &list)
	if status == http.StatusUnauthorized {
		// The session lapsed since the user id was cached: replace it once.
		g.cfg.Session.Invalidate(login, cookie)
		fresh, cerr := g.cfg.Session.Cookie(ctx, login)
		if cerr != nil {
			return
		}
		cookie = fresh
		status, err = c.hubGet(ctx, cookie, "/api/v1/chat/dms", &list)
	}
	if err != nil || status != http.StatusOK || list.DMs == nil {
		return
	}
	dms := map[string]hubDM{}
	for _, d := range *list.DMs {
		dms[d.ConversationKey] = d
	}
	reads := 0
	for i := range ans.Agents {
		e := &ans.Agents[i]
		if e.Access != accessMessage || e.Conversation == "" {
			continue
		}
		d, ok := dms[e.Conversation]
		if !ok || !d.HasUnread {
			zero := 0
			e.Unread = &zero
			continue
		}
		if reads >= maxUnreadReads {
			continue
		}
		reads++
		var page struct {
			Messages []hubMessage `json:"messages"`
			Items    []hubMessage `json:"items"`
		}
		p := "/api/v1/chat/conversations/" + url.PathEscape(e.Conversation) + fmt.Sprintf("/messages?limit=%d", unreadPage)
		if st, err := c.hubGet(ctx, cookie, p, &page); err != nil || st != http.StatusOK {
			continue
		}
		items := page.Messages
		if items == nil {
			items = page.Items
		}
		var keep func(hubMessage) bool
		if g.cfg.MatchAgentMessages != nil && ans.Tier == chatledger.TierContact {
			// Agent messages on: a contact's count holds only the agent rows
			// it is shown (agentmsgs.go); no answer, no count.
			rows := make([]historyRow, len(items))
			for i, m := range items {
				rows[i] = historyRow{ID: m.ID, Sender: m.Sender, SenderID: m.SenderID, Type: m.Type, Msg: m.Msg, CreatedAt: m.CreatedAt}
			}
			kept, err := g.keepAgentRows(ctx, login, e.Name, e.ID, ans.UserID, rows)
			if err != nil {
				continue
			}
			keep = func(m hubMessage) bool { return kept[m.ID] }
		}
		n := countUnread(items, e.ID, d.LastReadMessageID, len(items) >= unreadPage, keep)
		e.Unread = &n
	}
}

// errBadUserID means the hub named a user id the page cannot use.
var errBadUserID = errors.New("the hub named an unusable user id")

// hubUserID is the login's hub user id (cached for contactCacheTTL), asked
// of the hub with its own session; a 401 replaces the session once. It
// returns the cookie to use for further calls (the replacement, if any).
func (g *gate) hubUserID(ctx context.Context, login, cookie string) (string, string, error) {
	c := g.chat
	now := c.now
	c.mu.Lock()
	u, ok := c.users[login]
	c.mu.Unlock()
	if ok && now().Sub(u.at) < contactCacheTTL {
		return u.id, cookie, nil
	}
	uid, err := c.whoAmI(ctx, cookie)
	if errors.Is(err, errSessionUnknown) {
		// The hub no longer knows this session (it restarted, or the
		// session lapsed): replace it once, as forward does for a GET.
		g.cfg.Session.Invalidate(login, cookie)
		if fresh, cerr := g.cfg.Session.Cookie(ctx, login); cerr == nil {
			cookie = fresh
			uid, err = c.whoAmI(ctx, fresh)
		}
	}
	if err != nil {
		return "", cookie, err
	}
	if !validHubID(uid) {
		return "", cookie, errBadUserID
	}
	c.mu.Lock()
	if c.users == nil {
		c.users = map[string]contactUser{}
	}
	c.users[login] = contactUser{id: uid, at: now()}
	c.mu.Unlock()
	return uid, cookie, nil
}

// records is Config.AgentRecords, nil-safe: no source is an error (every
// state reads "unknown").
func (g *gate) records(ctx context.Context) (map[string]AgentRecord, error) {
	if g.cfg.AgentRecords == nil {
		return nil, errors.New("no agent records source")
	}
	return g.cfg.AgentRecords(ctx)
}

// labels is Config.Labels, nil-safe.
func (g *gate) labels() map[string]string {
	if g.cfg.Labels == nil {
		return nil
	}
	return g.cfg.Labels()
}

// agentsAnswerTTL is how long a login's list answer is reused. One list
// costs up to ten hub calls with the login's own session; the page asks
// every few seconds at most, so a burst gets the answer of a moment ago.
const agentsAnswerTTL = 2 * time.Second

type cachedAnswer struct {
	body []byte
	at   time.Time
}

// cachedAgents is login's answer of less than agentsAnswerTTL ago, if any.
// Keyed by login alone: an answer is never handed to another login.
func (c *chatPage) cachedAgents(login string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.answers[login]
	if !ok || c.now().Sub(a.at) >= agentsAnswerTTL {
		return nil, false
	}
	return a.body, true
}

func (c *chatPage) storeAgents(login string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answers == nil {
		c.answers = map[string]cachedAnswer{}
	}
	now := c.now()
	// Old answers go: the map holds at most the logins of the last moment.
	maps.DeleteFunc(c.answers, func(_ string, a cachedAnswer) bool { return now.Sub(a.at) >= agentsAnswerTTL })
	c.answers[login] = cachedAnswer{body: body, at: now}
}

func (c *chatPage) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// serveAgents answers GET /lever/api/agents for v.
func (g *gate) serveAgents(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, cookie string) {
	answer := func(body []byte) {
		g.answerChat(w, line, DecisionAllow, http.StatusOK, func() {
			w.Header().Set("Content-Type", "application/json")
			// Like every /api/ answer the proxy forwards (sandboxAPIDocument):
			// opened as a page, this is an inert document.
			w.Header().Set("Content-Security-Policy", "sandbox")
		}, body, r)
	}
	if body, ok := g.chat.cachedAgents(v.login); ok {
		answer(body)
		return
	}
	ctx := r.Context()
	uid, cookie, err := g.hubUserID(ctx, v.login, cookie)
	if err != nil {
		g.answerChat(w, line, DecisionChatUnavailable, http.StatusBadGateway, nil, []byte("cannot resolve your hub user\n"), r)
		return
	}
	recs, err := g.records(ctx)
	ans := buildAgents(v, g.cfg.ChatAgent, uid, recs, err != nil, g.labels(), g.cfg.ContactSession)
	if g.files != nil {
		ans.Files = &filesInfo{MaxBytes: g.files.cfg.MaxBytes, Extensions: g.files.cfg.Extensions}
	}
	g.addUnread(ctx, v.login, cookie, &ans)
	body, err := json.Marshal(ans)
	if err != nil {
		g.answerChat(w, line, DecisionChatUnavailable, http.StatusBadGateway, nil, []byte("cannot encode the agent list\n"), r)
		return
	}
	g.chat.storeAgents(v.login, body)
	answer(body)
}

// hubGetJSON reads a hub route with a login's own session and decodes a
// 200 answer (at most 1 MiB) into out. It reports the status; a non-200
// answer is not decoded.
func hubGetJSON(cfg Config) func(ctx context.Context, cookie, path string, out any) (int, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	if cfg.DialContext != nil {
		client.Transport = jailTransport(cfg.DialContext)
	}
	return func(ctx context.Context, cookie, path string, out any) (int, error) {
		ref, err := url.Parse(path)
		if err != nil {
			return 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Target.ResolveReference(ref).String(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Cookie", sessionCookieName+"="+cookie)
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
}
