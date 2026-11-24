package remoteproxy

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// Agents spawned from a template (`lever agent spawn`) come and go while the
// proxy runs, so they are not in Config.Workers: the proxy asks the broker's
// registry (Config.Spawned) and keeps the answer a few seconds. Only the
// operator's logins see them: a contact's lists name config agents only.

// SpawnedAgent is one registry entry as the proxy may use it.
type SpawnedAgent struct {
	Name, Template, Label string
	// State is the registry's: "spawning", "active", "destroying" or
	// "destroyed". Only an active agent is listed, messaged or woken.
	State string
	// Orphaned: its template left the config; the broker drives it no more.
	Orphaned    bool
	DestroyedAt time.Time
	// Transcript is the conversation backup destroy took, or "".
	Transcript string
	// WaitingSince is when it began waiting at its prompt, as the broker's
	// agents loop saw it (zero: not waiting). A hint the agent reports.
	WaitingSince time.Time
}

// SpawnedActive is the registry state of an agent that may be driven.
const (
	SpawnedActive    = "active"
	SpawnedDestroyed = "destroyed"
)

// spawnedTTL: one list is a request on the broker's local socket.
const spawnedTTL = 3 * time.Second

// spawnedName is the shape of an agent name the page accepts (chatcore.js
// hashAgent): the broker's names fit it, and anything else is dropped.
var spawnedName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type spawnedCache struct {
	mu sync.Mutex
	at time.Time // the last read, good or failed
	// good is when list was last read; a failed read keeps it for
	// spawnedGrace, then lists none.
	good     time.Time
	list     []SpawnedAgent
	inflight chan struct{} // closed when the read under way ends
}

// spawnedGrace: how long a list stays served while the broker fails to
// answer.
const spawnedGrace = 30 * time.Second

// spawnedReadTimeout bounds one read of the local socket. The read is
// detached from the request, so a client that goes away never fails it.
const spawnedReadTimeout = 2 * time.Second

// spawned lists the registry, cached for spawnedTTL. One read at a time, out
// of the lock: meanwhile callers get the list in hand, and only the first
// callers (no list yet) wait, for at most their own request. A failed read
// keeps the last list for spawnedGrace and then lists none: a spawned agent
// then drops out of the page until the broker answers, and no config agent
// is affected.
func (g *gate) spawned(ctx context.Context) []SpawnedAgent {
	if g.cfg.Spawned == nil {
		return nil
	}
	c := &g.spawnedList
	c.mu.Lock()
	now := time.Now()
	if !c.at.IsZero() && now.Sub(c.at) < spawnedTTL {
		defer c.mu.Unlock()
		return c.list
	}
	if ch := c.inflight; ch != nil {
		if !c.at.IsZero() {
			defer c.mu.Unlock()
			return c.list
		}
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.list
	}
	ch := make(chan struct{})
	c.inflight = ch
	c.mu.Unlock()

	out, err := g.readSpawned(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	now = time.Now()
	c.at = now
	switch {
	case err == nil:
		c.list, c.good = out, now
	case now.Sub(c.good) >= spawnedGrace:
		c.list = nil
	}
	c.inflight = nil
	close(ch)
	return c.list
}

// spawnedStale reports whether the last registry read failed: what spawned
// returns is then the kept list, or none past the grace.
func (g *gate) spawnedStale() bool {
	c := &g.spawnedList
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.at.Equal(c.good)
}

// readSpawned is one read of the registry, filtered. A panic in the source
// is an error, so the read under way always ends (spawned).
func (g *gate) readSpawned(ctx context.Context) (out []SpawnedAgent, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("registry read panicked: %v", p)
		}
	}()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spawnedReadTimeout)
	defer cancel()
	list, err := g.cfg.Spawned(rctx)
	if err != nil {
		return nil, err
	}
	out = make([]SpawnedAgent, 0, len(list))
	for _, a := range list {
		// A spawned agent never shadows a config agent, and only names the
		// page can carry pass.
		if !spawnedName.MatchString(a.Name) || a.Name == g.cfg.ChatAgent || slices.Contains(g.cfg.Workers, a.Name) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// liveSpawned is the spawned agents the operator may drive, in registry
// (spawn) order.
func (g *gate) liveSpawned(ctx context.Context) []SpawnedAgent {
	var out []SpawnedAgent
	for _, a := range g.spawned(ctx) {
		if a.State == SpawnedActive && !a.Orphaned {
			out = append(out, a)
		}
	}
	return out
}

// spawnedNamed finds name in the registry (any state).
func (g *gate) spawnedNamed(ctx context.Context, name string) (SpawnedAgent, bool) {
	for _, a := range g.spawned(ctx) {
		if a.Name == name {
			return a, true
		}
	}
	return SpawnedAgent{}, false
}

// spawnedRecord reads one record from the broker, destroyed ones included,
// bounded and detached from the request as readSpawned is. A failed read is
// no record.
func (g *gate) spawnedRecord(ctx context.Context, name string) (a SpawnedAgent, ok bool) {
	if g.cfg.SpawnedRecord == nil || !spawnedName.MatchString(name) || name == g.cfg.ChatAgent || slices.Contains(g.cfg.Workers, name) {
		return SpawnedAgent{}, false
	}
	defer func() {
		if p := recover(); p != nil {
			a, ok = SpawnedAgent{}, false
		}
	}()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spawnedReadTimeout)
	defer cancel()
	a, ok, err := g.cfg.SpawnedRecord(rctx, name)
	if err != nil || !ok || a.Name != name {
		return SpawnedAgent{}, false
	}
	return a, true
}

// isWorker: a config worker, or a live spawned agent.
func (g *gate) isWorker(ctx context.Context, name string) bool {
	if slices.Contains(g.cfg.Workers, name) {
		return true
	}
	return slices.ContainsFunc(g.liveSpawned(ctx), func(a SpawnedAgent) bool { return a.Name == name })
}

// spawnedInfo is the template and label of each live spawned agent, for the
// agent list.
func spawnedInfo(list []SpawnedAgent) map[string]SpawnedAgent {
	out := make(map[string]SpawnedAgent, len(list))
	for _, a := range list {
		out[a.Name] = a
	}
	return out
}

// The stable links: GET /lever/go/<name>/chat and /lever/go/<name>/terminal.
// <name> must match spawnedName, so a configured agent whose name is outside
// it (an upper-case letter, an underscore) has no stable link.
// Names are never reused, so a link stays the same agent's for its whole
// life, and after its destroy it says so. The route is an entry page (a link
// from another site lands on it), changes nothing and forbids framing.
const goPrefix = "/lever/go/"

// goTarget splits /lever/go/<name>/<view>; under reports the prefix.
func goTarget(p string) (name, view string, under bool) {
	if p != strings.TrimSuffix(goPrefix, "/") && !strings.HasPrefix(p, goPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, goPrefix)
	name, view, ok := strings.Cut(rest, "/")
	if !ok || !spawnedName.MatchString(name) || (view != "chat" && view != "terminal") {
		return "", "", true
	}
	return name, view, true
}

// serveGo answers a stable link. Every name the login may not reach, and
// every name that does not exist, gets the same not-found. The terminal is
// the operator's only; the chat opens for any agent the login may message.
func (g *gate) serveGo(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, name, view string) {
	notFound := func() {
		g.goPage(w, r, line, http.StatusNotFound, "Not found", "There is no agent here you can open.")
	}
	if name == "" {
		notFound()
		return
	}
	op := v.tier == chatledger.TierOperator
	if !v.mayMessage(name) {
		if !op {
			notFound()
			return
		}
		if a, ok := g.spawnedRecord(r.Context(), name); ok && a.State == SpawnedDestroyed {
			msg := "Agent " + name + " was destroyed"
			if !a.DestroyedAt.IsZero() {
				msg += " at " + a.DestroyedAt.UTC().Format("2006-01-02 15:04 UTC")
			}
			msg += "."
			if a.Transcript != "" {
				msg += " Its conversation was backed up as " + a.Transcript + "."
			}
			g.goPage(w, r, line, http.StatusGone, "Agent destroyed", msg)
			return
		}
		notFound()
		return
	}
	if view == "chat" {
		// A fixed prefix and a checked name: nothing else of the request goes
		// into Location.
		g.answerChat(w, line, DecisionAllow, http.StatusSeeOther, func() {
			w.Header().Set("Location", chatPagePath+"#agent="+name)
		}, nil, r)
		return
	}
	if !op {
		notFound()
		return
	}
	recs, err := g.records(r.Context())
	rec, found := recs[name]
	if err != nil || !found || !validHubID(rec.ID) {
		g.goPage(w, r, line, http.StatusConflict, "No terminal yet",
			"Agent "+name+" has no running session to open. Open its chat to wake it.")
		return
	}
	target := "/agents/" + rec.ID + "/terminal"
	if sameOriginArrival(r) {
		g.answerChat(w, line, DecisionAllow, http.StatusSeeOther, func() { w.Header().Set("Location", target) }, nil, r)
		return
	}
	// The hub's terminal (a live shell) is not an entry page. A link from
	// another site gets a page whose link the operator follows with their
	// own click: never an automatic hop, which would make the terminal one.
	g.goPage(w, r, line, http.StatusOK, "Open terminal", "The terminal of agent "+name+" is a live session: open it only from a link you trust.", target)
}

// sameOriginArrival: the request came from this origin's own page
// (Sec-Fetch-Site exactly same-origin). "none" is also a link from a mail or
// chat app, a bookmark or a typed address, which gets the Open it page; a
// missing or repeated header reads as cross-site.
func sameOriginArrival(r *http.Request) bool {
	v := r.Header.Values("Sec-Fetch-Site")
	return len(v) == 1 && v[0] == "same-origin"
}

// goPage is a small page with no script. Every text is escaped; link, when
// given, is a same-origin path built from checked parts.
func (g *gate) goPage(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, title, msg string, link ...string) {
	var b strings.Builder
	// No inline style: the chat page's CSP refuses it; the stylesheet is the
	// chat page's own.
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<link rel="stylesheet" href="/lever/chat.css">`)
	b.WriteString(`<title>` + html.EscapeString(title) + `</title></head><body><main class="go">`)
	b.WriteString(`<h1>` + html.EscapeString(title) + `</h1><p>` + html.EscapeString(msg) + `</p>`)
	if len(link) == 1 {
		b.WriteString(`<p><a href="` + html.EscapeString(link[0]) + `">Open it</a></p>`)
	}
	b.WriteString(`<p><a href="` + chatPagePath + `">Chat</a></p></main></body></html>`)
	g.answerChat(w, line, DecisionAllow, status, func() {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}, []byte(b.String()), r)
}
