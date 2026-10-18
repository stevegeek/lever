package remoteproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// execHub answers the hub's exec route for the chat test config and records
// each call's path, cookie and body.
type execHub struct {
	mu     sync.Mutex
	calls  []string
	bodies []string
	status int
	answer string
}

func (e *execHub) route(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/exec") {
		return false
	}
	b, _ := io.ReadAll(r.Body)
	c, _ := r.Cookie(sessionCookieName)
	e.mu.Lock()
	e.calls = append(e.calls, r.Method+" "+r.URL.Path+" "+c.Value)
	e.bodies = append(e.bodies, string(b))
	status, answer := e.status, e.answer
	e.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
	return true
}

func (e *execHub) got() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

func (e *execHub) set(status int, answer string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.answer = status, answer
}

func execJSON(output string, code int) string {
	b, _ := json.Marshal(execAnswer{Output: output, ExitCode: code})
	return string(b)
}

func activityHub(t *testing.T) (*pageHub, *execHub) {
	hub := newPageHub(t)
	ex := &execHub{answer: execJSON("hello\n", 0)}
	hub.route = ex.route
	return hub, ex
}

func activityPost(h http.Handler, login, name string, hdr ...string) *httptest.ResponseRecorder {
	return chatDo(h, login, "POST", "/lever/api/agents/"+name+"/activity",
		append([]string{"Origin", "https://" + testServeHost, "Sec-Fetch-Site", "same-origin"}, hdr...)...)
}

func TestActivityForAnOperator(t *testing.T) {
	hub, ex := activityHub(t)
	ex.set(http.StatusOK, execJSON("\x1b[1;32m● Bash(go test ./...)\x1b[0m   \r\n  <b>ok</b> \u202eevil\n\n\n", 0))
	var lines lockedLines
	cfg := chatConfig(t, hub)
	cfg.Audit = lines.add
	rw := activityPost(NewHandler(cfg), chatOp, "w1")
	if rw.Code != http.StatusOK {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store", "Content-Security-Policy": "sandbox", "X-Content-Type-Options": "nosniff"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	var ans activityAnswer
	if err := json.Unmarshal(rw.Body.Bytes(), &ans); err != nil {
		t.Fatal(err)
	}
	if want := "● Bash(go test ./...)\n  <b>ok</b> evil"; ans.Text != want || ans.Truncated {
		t.Fatalf("text %q truncated %v, want %q", ans.Text, ans.Truncated, want)
	}
	// The markup is JSON-escaped on the wire as well as text on the page.
	if strings.Contains(rw.Body.String(), "<b>") {
		t.Errorf("raw markup in the answer: %s", rw.Body)
	}
	if got := ex.got(); !slices.Equal(got, []string{"POST /api/v1/agents/" + agentW1 + "/exec " + testCookie}) {
		t.Fatalf("hub calls %v", got)
	}
	var sent struct {
		Command []string `json:"command"`
		Timeout int      `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(ex.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if want := []string{"tmux", "capture-pane", "-p", "-J", "-t", "scion:agent", "-S", "-200"}; !slices.Equal(sent.Command, want) || sent.Timeout != 10 {
		t.Fatalf("command %q timeout %d", sent.Command, sent.Timeout)
	}
	all := lines.all()
	last := all[len(all)-1]
	if last.Decision != DecisionActivity || last.Status != http.StatusOK || last.Agent != "w1" || last.TSLogin != chatOp {
		t.Fatalf("audit %+v", last)
	}
	if b, _ := json.Marshal(all); strings.Contains(string(b), "go test") {
		t.Fatalf("pane text in the audit: %s", b)
	}
	// The manager too.
	if rw := activityPost(NewHandler(cfg), chatOp, "boss"); rw.Code != http.StatusOK {
		t.Fatalf("manager: %d", rw.Code)
	}
}

func TestActivityRefusesContactsAndUnknownNames(t *testing.T) {
	hub, ex := activityHub(t)
	var lines lockedLines
	cfg := chatConfig(t, hub)
	cfg.Audit = lines.add
	h := NewHandler(cfg)
	var first string
	// c@x may message w1 and see w2; neither opens its pane.
	for _, tc := range []struct{ login, name string }{
		{"c@x", "w1"}, {"c@x", "w2"}, {"c@x", "boss"}, {"c@x", "nope"}, {chatOp, "nope"},
	} {
		rw := activityPost(h, tc.login, tc.name)
		if rw.Code != http.StatusForbidden {
			t.Errorf("%s reads %s: %d", tc.login, tc.name, rw.Code)
		}
		if first == "" {
			first = rw.Body.String()
		} else if rw.Body.String() != first {
			t.Errorf("%s reads %s: body %q differs from %q", tc.login, tc.name, rw.Body, first)
		}
	}
	if strings.TrimSpace(first) != `{"error":"not-allowed"}` {
		t.Errorf("body %q", first)
	}
	for _, l := range lines.all() {
		if l.Decision != DecisionDenyActivity || l.Reason != "not-allowed" || l.Agent != "" {
			t.Errorf("audit %+v", l)
		}
	}
	if got := ex.got(); len(got) != 0 {
		t.Fatalf("hub exec called: %v", got)
	}
}

func TestActivityOriginAndMethod(t *testing.T) {
	hub, ex := activityHub(t)
	h := NewHandler(chatConfig(t, hub))
	for name, hdr := range map[string][]string{
		"no origin":    {"Origin", ""},
		"cross origin": {"Origin", "https://evil.test"},
		"cross site":   {"Sec-Fetch-Site", "cross-site"},
		"same-site":    {"Sec-Fetch-Site", "same-site"},
	} {
		req := proxyRequest("POST", "/lever/api/agents/w1/activity", nil)
		req.Header.Set("Tailscale-User-Login", chatOp)
		req.Header.Set("Origin", "https://"+testServeHost)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if hdr[1] == "" {
			req.Header.Del(hdr[0])
		} else {
			req.Header.Set(hdr[0], hdr[1])
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rw.Code)
		}
	}
	for _, m := range []string{"GET", "HEAD", "PUT", "DELETE"} {
		if rw := chatDo(h, chatOp, m, "/lever/api/agents/w1/activity", "Origin", "https://"+testServeHost); rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d Allow %q", m, rw.Code, rw.Header().Get("Allow"))
		}
	}
	for _, p := range []string{"/lever/api/agents/W1/activity", "/lever/api/agents/w1/activity/", "/lever/api/agents/w1/activityx",
		"/lever/api/agents//activity", "/lever/api/agents/w1/x/activity", "/lever/api/agents/../agents/w1/activity"} {
		rw := chatDo(h, chatOp, "POST", p, "Origin", "https://"+testServeHost, "Sec-Fetch-Site", "same-origin")
		if rw.Code == http.StatusOK {
			t.Errorf("%s: %d", p, rw.Code)
		}
	}
	if got := ex.got(); len(got) != 0 {
		t.Fatalf("hub exec called: %v", got)
	}
}

func TestActivityNeedsARunningAgent(t *testing.T) {
	hub, ex := activityHub(t)
	h := NewHandler(chatConfig(t, hub))
	rw := activityPost(h, chatOp, "w3") // suspended
	if rw.Code != http.StatusConflict || strings.TrimSpace(rw.Body.String()) != `{"error":"not-running","state":"suspended"}` {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if got := ex.got(); len(got) != 0 {
		t.Fatalf("hub exec called: %v", got)
	}
}

func TestActivityRateLimit(t *testing.T) {
	hub, ex := activityHub(t)
	g := NewHandler(chatConfig(t, hub)).(*gate)
	now := time.Unix(1000, 0)
	g.activity.now = func() time.Time { return now }
	if rw := activityPost(g, chatOp, "w1"); rw.Code != http.StatusOK {
		t.Fatalf("first: %d", rw.Code)
	}
	now = now.Add(500 * time.Millisecond)
	rw := activityPost(g, chatOp, "w1")
	if rw.Code != http.StatusTooManyRequests || rw.Header().Get("Retry-After") != "2" {
		t.Fatalf("second: %d Retry-After %q", rw.Code, rw.Header().Get("Retry-After"))
	}
	if rw := activityPost(g, chatOp, "w2"); rw.Code != http.StatusOK {
		t.Fatalf("another agent: %d", rw.Code)
	}
	now = now.Add(activityEvery)
	if rw := activityPost(g, chatOp, "w1"); rw.Code != http.StatusOK {
		t.Fatalf("after the gap: %d", rw.Code)
	}
	if n := len(ex.got()); n != 3 {
		t.Fatalf("%d hub calls", n)
	}
	// In flight: a third concurrent capture waits for one to end.
	l := &activityLimiter{now: func() time.Time { return now }}
	for _, n := range []string{"a", "b"} {
		if ok, _ := l.take(n); !ok {
			t.Fatalf("take %s", n)
		}
	}
	if ok, _ := l.take("c"); ok {
		t.Fatal("third concurrent capture admitted")
	}
	l.release("a")
	if ok, _ := l.take("c"); !ok {
		t.Fatal("refused after a release")
	}
	// An agent whose capture is still under way gets no second one, whatever
	// the time.
	now = now.Add(time.Minute)
	if ok, _ := l.take("b"); ok {
		t.Fatal("a second capture of a busy agent")
	}
	l.release("b")
	if ok, _ := l.take("b"); !ok {
		t.Fatal("refused after its capture ended")
	}
}

func TestActivityHubFailures(t *testing.T) {
	big := execJSON(strings.Repeat("x", activityMaxHub), 0)
	for _, tc := range []struct {
		name, answer, want string
		status, code       int
	}{
		{"exit code", execJSON("secret-ish agent text", 1), `{"error":"unavailable"}`, http.StatusOK, http.StatusBadGateway},
		{"hub 500", `{"error":{"message":"agent text"}}`, `{"error":"unavailable"}`, http.StatusInternalServerError, http.StatusBadGateway},
		{"hub 409", `{}`, `{"error":"not-running","state":"error"}`, http.StatusConflict, http.StatusConflict},
		{"hub 403", `{}`, `{"error":"hub-refused"}`, http.StatusForbidden, http.StatusForbidden},
		{"not json", `<html>`, `{"error":"unavailable"}`, http.StatusOK, http.StatusBadGateway},
		{"too big", big, `{"error":"too-large"}`, http.StatusOK, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, ex := activityHub(t)
			ex.set(tc.status, tc.answer)
			rw := activityPost(NewHandler(chatConfig(t, hub)), chatOp, "w1")
			if rw.Code != tc.code || strings.TrimSpace(rw.Body.String()) != tc.want {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
		})
	}
}

func TestActivityRenewsAnUnknownSession(t *testing.T) {
	hub, ex := activityHub(t)
	ex.set(http.StatusUnauthorized, `{}`)
	sess := testSession()
	cfg := chatConfig(t, hub)
	cfg.Session = sess
	rw := activityPost(NewHandler(cfg), chatOp, "w1")
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if _, inv, _ := sess.state(); !slices.Contains(inv, testCookie) {
		t.Fatalf("not invalidated: %v", inv)
	}
	if n := len(ex.got()); n != 2 {
		t.Fatalf("%d hub calls, want 2", n)
	}
}

func TestCleanActivity(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "a\nb", "a\nb"},
		{"csi", "\x1b[31mred\x1b[0m \x1b[2J\x1b[?1049h", "red"},
		{"osc bel", "x\x1b]0;title\x07y", "xy"},
		{"osc st", "x\x1b]8;;https://evil.test\x1b\\link\x1b]8;;\x1b\\y", "xlinky"},
		{"dcs", "x\x1bP1;2|data\x1b\\y", "xy"},
		{"c1 csi", "x\u009b31my", "xy"},
		{"c1 osc", "x\u009d0;t\u009cy", "xy"},
		{"two char", "x\x1b(By\x1b7z", "xyz"},
		{"controls", "a\x00b\x08c\rd\x7fe\u0085f", "abcdef"},
		{"bidi", "a\u202eb\u2066c\u200fd\u2028e\ufefff", "abcdef"},
		{"tab kept", "a\tb", "a\tb"},
		{"trailing blanks", "  \n\na  \t\nb \n \n\n", "a\nb"},
		{"markup is text", "<img src=x onerror=alert(1)>", "<img src=x onerror=alert(1)>"},
		{"unterminated osc ends with its line", "a\x1b]0;forever\nnext", "a\nnext"},
		{"8-bit osc cannot hide later lines", "line1\nosc:\u009d hidden?\ncsi:\u009b x\nTOOL CALL: rm -rf /\nlast\n", "line1\nosc:\ncsi:\nTOOL CALL: rm -rf /\nlast"},
		{"8-bit csi keeps the newlines", "a\u009b\n\n\n1234\nZZ\n", "a\n\n\n1234\nZZ"},
		{"dcs start alone", "a\u0090b\nc", "a\nc"},
		{"bare escape keeps its newline", "a\x1b\nb", "a\nb"},
		{"long sequence ends", "a\x1b]" + strings.Repeat("x", activityEscapeRunes-1) + "visible", "avisible"},
		{"other format characters", "a\u00adb\u200dc\U000e0041d\u180ee", "abcde"},
	} {
		got, trunc := cleanActivity(tc.in)
		if got != tc.want || trunc {
			t.Errorf("%s: %q (truncated %v), want %q", tc.name, got, trunc, tc.want)
		}
	}
	// Lines: the last activityMaxLines.
	var b strings.Builder
	for i := range activityMaxLines + 50 {
		b.WriteString("l")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	got, trunc := cleanActivity(b.String())
	if n := strings.Count(got, "\n") + 1; n != activityMaxLines || !trunc {
		t.Fatalf("%d lines truncated %v", n, trunc)
	}
	// One long line is cut by runes, never inside a character.
	got, trunc = cleanActivity(strings.Repeat("é", activityLineRunes+10))
	if utf8.RuneCountInString(got) != activityLineRunes+1 || !strings.HasSuffix(got, "…") || !trunc || !utf8.ValidString(got) {
		t.Fatalf("long line: %d runes truncated %v", utf8.RuneCountInString(got), trunc)
	}
	// Bytes: the tail that fits.
	line := strings.Repeat("y", 1000)
	got, trunc = cleanActivity(strings.Repeat(line+"\n", 200) + "last")
	if len(got) > activityMaxBytes || !strings.HasSuffix(got, "\nlast") || !trunc {
		t.Fatalf("bytes: %d truncated %v", len(got), trunc)
	}
	// Invalid UTF-8 never survives.
	if got, _ := cleanActivity("a\xffb"); !utf8.ValidString(got) {
		t.Fatalf("invalid utf-8: %q", got)
	}
}
