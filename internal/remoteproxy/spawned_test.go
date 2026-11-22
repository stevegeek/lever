package remoteproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const spawnedID = "id-s-abc234"

// spawnedConfig is chatConfig with a registry: s-abc234 live (and suspended
// in the hub), s-new222 still spawning, s-old777 destroyed, s-orph55 with
// its template gone, and a registry entry named like the worker w1.
func spawnedConfig(t *testing.T, hub *pageHub, calls *atomic.Int32) Config {
	cfg := chatConfig(t, hub)
	recs := cfg.AgentRecords
	cfg.AgentRecords = func(ctx context.Context) (map[string]AgentRecord, error) {
		m, err := recs(ctx)
		m["s-abc234"] = AgentRecord{ID: spawnedID, Phase: "suspended"}
		return m, err
	}
	cfg.Spawned = func(context.Context) ([]SpawnedAgent, error) {
		if calls != nil {
			calls.Add(1)
		}
		return []SpawnedAgent{
			{Name: "s-abc234", Template: "session", Label: "Fix the build", State: SpawnedActive},
			{Name: "s-new222", Template: "session", State: "spawning"},
			{Name: "s-old777", Template: "session", State: SpawnedDestroyed,
				DestroyedAt: time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC), Transcript: "20261120-093000-ab12"},
			{Name: "s-orph55", Template: "gone", State: SpawnedActive, Orphaned: true},
			{Name: "w1", Template: "session", State: SpawnedActive},
			{Name: "Bad<Name>", Template: "session", State: SpawnedActive},
		}, nil
	}
	return cfg
}

func names(ans agentsAnswer) []string {
	var out []string
	for _, a := range ans.Agents {
		out = append(out, a.Name)
	}
	return out
}

func TestSpawnedAgentsListedForTheOperatorOnly(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(spawnedConfig(t, hub, nil))
	rw, ans := agentsAs(t, h, chatOp)
	if rw.Code != http.StatusOK {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if got := names(ans); !slices.Equal(got, []string{"boss", "w1", "w2", "w3", "s-abc234"}) {
		t.Fatalf("operator roster %v", got)
	}
	s := ans.Agents[4]
	if s.Template != "session" || s.Label != "Fix the build" || s.Role != "worker" || s.State != "suspended" ||
		s.Terminal != "/agents/"+spawnedID+"/terminal" {
		t.Fatalf("spawned row %+v", s)
	}
	if ans.Agents[1].Template != "" {
		t.Fatalf("a config worker carries a template: %+v", ans.Agents[1])
	}
	_, ans = agentsAs(t, h, "c@x")
	if got := names(ans); slices.Contains(got, "s-abc234") {
		t.Fatalf("a contact sees a spawned agent: %v", got)
	}
}

func TestSpawnedLabelFallsBackToTheTemplate(t *testing.T) {
	ans := agentsAnswer{Agents: []agentEntry{{Name: "s-a"}, {Name: "s-b", Label: "from labels file"}, {Name: "w1"}}}
	markSpawned(&ans, map[string]SpawnedAgent{"s-a": {Name: "s-a", Template: "t"}, "s-b": {Name: "s-b", Template: "t", Label: "x"}})
	if ans.Agents[0].Label != "from template t" || ans.Agents[1].Label != "from labels file" || ans.Agents[2].Template != "" {
		t.Fatalf("%+v", ans.Agents)
	}
}

func TestSpawnedListIsCachedAndFailsToNone(t *testing.T) {
	hub := newPageHub(t)
	var calls atomic.Int32
	cfg := spawnedConfig(t, hub, &calls)
	g := NewHandler(cfg).(*gate)
	for range 3 {
		g.viewerFor(context.Background(), chatOp)
	}
	if calls.Load() != 1 {
		t.Fatalf("registry read %d times, want 1 within the TTL", calls.Load())
	}
	// A failed read keeps the last list for the grace period, then lists
	// none; the config roster never changes.
	good := cfg.Spawned
	var fail atomic.Bool
	cfg.Spawned = func(ctx context.Context) ([]SpawnedAgent, error) {
		if fail.Load() {
			return nil, errors.New("broker down")
		}
		return good(ctx)
	}
	g = NewHandler(cfg).(*gate)
	g.viewerFor(context.Background(), chatOp)
	fail.Store(true)
	g.spawnedList.at = g.spawnedList.at.Add(-spawnedTTL)
	if v := g.viewerFor(context.Background(), chatOp); !slices.Contains(v.message, "s-abc234") {
		t.Fatalf("a failed read within the grace dropped the list: %v", v.message)
	}
	g.spawnedList.at = g.spawnedList.at.Add(-spawnedTTL)
	g.spawnedList.good = g.spawnedList.good.Add(-spawnedGrace)
	if v := g.viewerFor(context.Background(), chatOp); !slices.Equal(v.message, []string{"boss", "w1", "w2", "w3"}) {
		t.Fatalf("after the grace: %v", v.message)
	}
	// A request whose client went away does not fail the read.
	fail.Store(false)
	g.spawnedList.at = g.spawnedList.at.Add(-spawnedTTL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.cfg.Spawned = func(ctx context.Context) ([]SpawnedAgent, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return good(ctx)
	}
	if v := g.viewerFor(ctx, chatOp); !slices.Contains(v.message, "s-abc234") {
		t.Fatalf("a cancelled request failed the read: %v", v.message)
	}
}

// The file exchange refuses a spawned agent with the answer of a hidden
// one, and the list tells the page so.
func TestFilesNotOfferedForSpawnedAgents(t *testing.T) {
	hub := newPageHub(t)
	cfg := spawnedConfig(t, hub, nil)
	cfg.Files = &FilesConfig{Tree: t.TempDir(), MaxBytes: 1 << 20, Extensions: []string{".txt"}, LedgerDir: t.TempDir(),
		Workspaces: map[string]string{"boss": ".", "w1": "workers/w1", "w2": "workers/w2", "w3": "workers/w3"}}
	h := NewHandler(cfg)
	_, ans := agentsAs(t, h, chatOp)
	for _, a := range ans.Agents {
		if a.NoFiles != (a.Name == "s-abc234") {
			t.Errorf("%s noFiles %v", a.Name, a.NoFiles)
		}
	}
	spawned := chatDo(h, chatOp, "GET", "/lever/api/files/s-abc234", "Sec-Fetch-Site", "same-origin")
	hidden := chatDo(h, chatOp, "GET", "/lever/api/files/zz", "Sec-Fetch-Site", "same-origin")
	if spawned.Code != http.StatusForbidden || spawned.Code != hidden.Code || spawned.Body.String() != hidden.Body.String() {
		t.Fatalf("spawned %d %q, hidden %d %q", spawned.Code, spawned.Body, hidden.Code, hidden.Body)
	}
}

// Push targets and the activity route take a live spawned agent like a
// worker.
func TestPushTargetsIncludeSpawnedAgents(t *testing.T) {
	hub := newPageHub(t)
	g := NewHandler(spawnedConfig(t, hub, nil)).(*gate)
	targets, err := g.pushTargets(context.Background(), chatOp, "u-op")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tg := range targets {
		got = append(got, tg.name)
	}
	if !slices.Contains(got, "s-abc234") || slices.Contains(got, "s-new222") {
		t.Fatalf("%v", got)
	}
	ct, _ := g.pushTargets(context.Background(), "c@x", "u-c")
	for _, tg := range ct {
		if strings.HasPrefix(tg.name, "s-") {
			t.Fatalf("a contact's push target %v", tg)
		}
	}
}

func TestWakeASpawnedAgent(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	cfg := spawnedConfig(t, hub, nil)
	cfg.Wake = wr.fn
	h := NewHandler(cfg)
	if rw := wakePost(h, chatOp, "s-abc234"); rw.Code != http.StatusAccepted {
		t.Fatalf("operator wake: %d %s", rw.Code, rw.Body)
	}
	for _, c := range []struct{ login, name string }{{"c@x", "s-abc234"}, {chatOp, "s-new222"}, {chatOp, "s-old777"}, {chatOp, "s-orph55"}} {
		if rw := wakePost(h, c.login, c.name); rw.Code != http.StatusForbidden {
			t.Errorf("%s wakes %s: %d", c.login, c.name, rw.Code)
		}
	}
	if got := wr.got(); !slices.Equal(got, []string{chatOp + " s-abc234"}) {
		t.Fatalf("broker calls %v", got)
	}
}

func TestGoTargetShapes(t *testing.T) {
	for _, c := range []struct {
		p, name, view string
		under         bool
	}{
		{"/lever/go/s-abc234/chat", "s-abc234", "chat", true},
		{"/lever/go/s-abc234/terminal", "s-abc234", "terminal", true},
		{"/lever/go/s-abc234", "", "", true},
		{"/lever/go/s-abc234/", "", "", true},
		{"/lever/go/s-abc234/chat/x", "", "", true},
		{"/lever/go/S-ABC/chat", "", "", true},
		{"/lever/go/../chat", "", "", true},
		{"/lever/go", "", "", true},
		{"/lever/gone", "", "", false},
		{"/lever/chat", "", "", false},
	} {
		name, view, under := goTarget(c.p)
		if name != c.name || view != c.view || under != c.under {
			t.Errorf("%s: %q %q %v", c.p, name, view, under)
		}
	}
}

func goGet(h http.Handler, login, p string) *httptest.ResponseRecorder {
	return chatDo(h, login, "GET", p, "Sec-Fetch-Site", "same-origin")
}

func TestGoLinks(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(spawnedConfig(t, hub, nil))

	rw := goGet(h, chatOp, "/lever/go/s-abc234/chat")
	if rw.Code != http.StatusSeeOther || rw.Header().Get("Location") != "/lever/chat#agent=s-abc234" {
		t.Fatalf("chat link: %d %q", rw.Code, rw.Header().Get("Location"))
	}
	if rw := goGet(h, chatOp, "/lever/go/w2/chat"); rw.Header().Get("Location") != "/lever/chat#agent=w2" {
		t.Fatalf("a config worker's chat link: %d", rw.Code)
	}

	// From the operator's own page, straight to the terminal.
	rw = goGet(h, chatOp, "/lever/go/s-abc234/terminal")
	if rw.Code != http.StatusSeeOther || rw.Header().Get("Location") != "/agents/"+spawnedID+"/terminal" {
		t.Fatalf("terminal link: %d %q", rw.Code, rw.Header().Get("Location"))
	}
	// With no Fetch Metadata (or a cross-site arrival): a page with a link
	// the operator follows, never an automatic hop.
	rw = chatDo(h, chatOp, "GET", "/lever/go/s-abc234/terminal")
	body := rw.Body.String()
	if rw.Code != http.StatusOK || !strings.Contains(body, `href="/agents/`+spawnedID+`/terminal"`) ||
		strings.Contains(body, "refresh") || strings.Contains(body, "<script") || strings.Contains(body, "style=") {
		t.Fatalf("terminal page: %d %s", rw.Code, body)
	}
	for k, v := range map[string]string{"X-Frame-Options": "DENY", "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if rw := chatDo(h, chatOp, "HEAD", "/lever/go/s-abc234/chat", "Sec-Fetch-Site", "same-origin"); rw.Code != http.StatusSeeOther || rw.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", rw.Code, rw.Body)
	}

	rw = goGet(h, chatOp, "/lever/go/s-old777/terminal")
	if rw.Code != http.StatusGone || !strings.Contains(rw.Body.String(), "2026-11-20 09:30 UTC") || !strings.Contains(rw.Body.String(), "20261120-093000-ab12") {
		t.Fatalf("destroyed: %d %s", rw.Code, rw.Body)
	}

	// One not-found for a name that does not exist, a spawning or orphaned
	// agent, a bad shape, and anything a contact may not reach.
	for _, c := range []struct{ login, p string }{
		{chatOp, "/lever/go/s-zzzzzz/chat"},
		{chatOp, "/lever/go/s-new222/chat"},
		{chatOp, "/lever/go/s-orph55/terminal"},
		{chatOp, "/lever/go/s-abc234/files"},
		{chatOp, "/lever/go/s-abc234"},
		{"c@x", "/lever/go/s-abc234/chat"},
		{"c@x", "/lever/go/s-old777/chat"},
		{"c@x", "/lever/go/w1/terminal"},
		{"c@x", "/lever/go/w2/chat"},
	} {
		if rw := goGet(h, c.login, c.p); rw.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", c.login, c.p, rw.Code)
		}
	}
	// A contact opens the chat of an agent it messages.
	if rw := goGet(h, "c@x", "/lever/go/w1/chat"); rw.Code != http.StatusSeeOther {
		t.Errorf("contact chat link: %d", rw.Code)
	}
	if rw := chatDo(h, chatOp, "POST", "/lever/go/s-abc234/chat", "Origin", "https://"+testServeHost, "Sec-Fetch-Site", "same-origin"); rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rw.Code)
	}
	for _, p := range hub.reached() {
		if strings.HasPrefix(p, "/lever/") {
			t.Errorf("forwarded to the hub: %s", p)
		}
	}
}

func TestGoTerminalWithNoRecord(t *testing.T) {
	hub := newPageHub(t)
	cfg := spawnedConfig(t, hub, nil)
	recs := cfg.AgentRecords
	cfg.AgentRecords = func(ctx context.Context) (map[string]AgentRecord, error) {
		m, err := recs(ctx)
		delete(m, "s-abc234")
		return m, err
	}
	if rw := goGet(NewHandler(cfg), chatOp, "/lever/go/s-abc234/terminal"); rw.Code != http.StatusConflict || strings.Contains(rw.Body.String(), "/agents/") {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

// A link on another site lands on a stable link (an entry page); any other
// shape under /lever/go/ stays refused cross-site.
func TestGoLinksAreEntryPages(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(spawnedConfig(t, hub, nil))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, navRequest("GET", "/lever/go/s-abc234/terminal", chatOp))
	if rw.Code != http.StatusOK || strings.Contains(rw.Body.String(), "refresh") || rw.Header().Get("Location") != "" {
		t.Fatalf("a link from another site hops to the terminal: %d %s", rw.Code, rw.Body)
	}
	for p, want := range map[string]int{
		"/lever/go/s-abc234/chat":     http.StatusSeeOther,
		"/lever/go/s-abc234/terminal": http.StatusOK,
		"/lever/go/s-abc234":          http.StatusForbidden,
		"/lever/go/s-abc234/x":        http.StatusForbidden,
		"/lever/go":                   http.StatusForbidden,
	} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, navRequest("GET", p, chatOp))
		if rw.Code != want {
			t.Errorf("%s: %d, want %d", p, rw.Code, want)
		}
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, navRequest("GET", "/lever/go/s-abc234/chat", chatOp, "Sec-Fetch-Dest", "iframe"))
	if rw.Code != http.StatusForbidden {
		t.Errorf("framed: %d", rw.Code)
	}
}
