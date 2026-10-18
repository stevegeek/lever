package remoteproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The recent-activity route: POST /lever/api/agents/<name>/activity answers
// an operator the last lines of the agent's Claude Code pane, as plain text.
//
// The proxy asks the hub to run one fixed command (tmux capture-pane, no
// escapes) in the agent's container, with the operator login's own hub
// session: the hub's exec route needs agent.attach, which the same login
// already uses for the Terminal link, so nothing here widens what the login
// can do. Nothing of the request reaches the command or the hub path but the
// agent's name, matched against config, and the hub id comes from the
// controller's records. A contact never gets here: its hub role has no
// agent.attach, and the proxy refuses it first.
//
// The pane holds whatever the agent printed. The answer is cleaned here
// (escape sequences, control and bidi characters gone) and bounded in lines
// and bytes; the page writes it as text only.

// Activity audit decisions. The pane's text never goes into the audit line.
const (
	DecisionActivity     Decision = "agent-activity"
	DecisionDenyActivity Decision = "deny-agent-activity"
)

const (
	chatActivitySuffix = "/activity"
	// activityRows is how far back into the pane's history the capture
	// starts; the visible screen comes on top.
	activityRows = 200
	// The cleaned answer keeps at most this many of the last lines, each cut
	// to activityLineRunes, and at most activityMaxBytes in all.
	activityMaxLines  = 400
	activityLineRunes = 2000
	activityMaxBytes  = 64 << 10
	// activityEscapeRunes bounds one escape sequence.
	activityEscapeRunes = 64
	// activityMaxHub bounds the hub's JSON answer: a wide pane times the
	// rows is far below it, and anything above is refused, not cut.
	activityMaxHub = 1 << 20
	// activityExecSeconds is the hub's own limit on the command.
	activityExecSeconds = 10
	activityBudget      = 20 * time.Second
	// One capture per agent per activityEvery, across logins, and at most
	// activityInFlight at a time: each one is a process in a container.
	activityEvery    = 2 * time.Second
	activityInFlight = 2
)

// activityCommand is the whole command. -J joins wrapped rows so the page can
// wrap them to its own width; without -e tmux writes no escape sequences.
var activityCommand = []string{"tmux", "capture-pane", "-p", "-J", "-t", "scion:agent", "-S", "-" + strconv.Itoa(activityRows)}

// activityTarget reports a well-formed activity path's agent name.
func activityTarget(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, chatAgentsPrefix)
	if !ok {
		return "", false
	}
	n, ok := strings.CutSuffix(rest, chatActivitySuffix)
	if !ok || !agentNameRE.MatchString(n) {
		return "", false
	}
	return n, true
}

type activityLimiter struct {
	mu       sync.Mutex
	last     map[string]time.Time
	busy     map[string]bool // agents with a capture under way
	inFlight int
	now      func() time.Time
}

// take reserves a capture of name; release must follow a true answer.
func (l *activityLimiter) take(name string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if t, seen := l.last[name]; seen && now.Sub(t) < activityEvery {
		return false, activityEvery - now.Sub(t)
	}
	// A capture that hangs holds its agent, never the other slot too.
	if l.busy[name] || l.inFlight >= activityInFlight {
		return false, activityEvery
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	maps.DeleteFunc(l.last, func(_ string, t time.Time) bool { return now.Sub(t) >= activityEvery })
	l.last[name] = now
	if l.busy == nil {
		l.busy = map[string]bool{}
	}
	l.busy[name] = true
	l.inFlight++
	return true, 0
}

func (l *activityLimiter) release(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, name)
	l.inFlight--
}

type activityAnswer struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (g *gate) serveActivity(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, name, cookie string) {
	refuse := func(status int, word string, extra map[string]string) {
		line.Reason = word
		body := map[string]string{"error": word}
		maps.Copy(body, extra)
		g.answerWakeJSON(w, r, line, DecisionDenyActivity, status, body)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(http.StatusMethodNotAllowed, "method", nil)
		return
	}
	// A POST, from the page only: each read runs a process in the agent's
	// container, and a cross-site form must not start one.
	if !sameOriginWrite(r, g.cfg.ServeHost) {
		refuse(http.StatusForbidden, "origin", nil)
		return
	}
	// One answer for a contact, a hidden agent and a name that does not
	// exist.
	if v.tier != chatledger.TierOperator || !v.mayMessage(name) {
		refuse(http.StatusForbidden, "not-allowed", nil)
		return
	}
	line.Agent = name
	if ok, wait := g.activity.take(name); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		refuse(http.StatusTooManyRequests, "rate-limited", nil)
		return
	}
	defer g.activity.release(name)
	ctx, cancel := context.WithTimeout(r.Context(), activityBudget)
	defer cancel()
	recs, err := g.records(ctx)
	rec, found := recs[name]
	if found && !validHubID(rec.ID) {
		rec, found, err = AgentRecord{}, false, errBadUserID
	}
	if state, _ := agentState(rec, found, err != nil); state != "running" {
		refuse(http.StatusConflict, "not-running", map[string]string{"state": state})
		return
	}
	status, out, err := g.chat.hubExec(ctx, cookie, rec.ID)
	if status == http.StatusUnauthorized {
		g.cfg.Session.Invalidate(v.login, cookie)
		if fresh, cerr := g.cfg.Session.Cookie(ctx, v.login); cerr == nil {
			status, out, err = g.chat.hubExec(ctx, fresh, rec.ID)
		}
	}
	switch {
	case errors.Is(err, errActivityTooBig):
		refuse(http.StatusBadGateway, "too-large", nil)
		return
	case err != nil:
		line.Error = truncateAudit(err.Error())
		refuse(http.StatusBadGateway, "unavailable", nil)
		return
	case status == http.StatusConflict:
		refuse(http.StatusConflict, "not-running", map[string]string{"state": "error"})
		return
	case status == http.StatusForbidden || status == http.StatusNotFound:
		refuse(http.StatusForbidden, "hub-refused", nil)
		return
	case status != http.StatusOK || out.ExitCode != 0:
		// The command's own output on a failure is the agent's text too, and
		// never a reason.
		refuse(http.StatusBadGateway, "unavailable", nil)
		return
	}
	text, truncated := cleanActivity(out.Output)
	body, _ := json.Marshal(activityAnswer{Text: text, Truncated: truncated})
	g.answerChat(w, line, DecisionActivity, http.StatusOK, func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, append(body, '\n'), r)
}

type execAnswer struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

var errActivityTooBig = errors.New("the hub's exec answer is over the limit")

// hubExecActivity posts activityCommand, and nothing else, to the hub's exec
// route for one agent id with a login's session.
func hubExecActivity(cfg Config) func(ctx context.Context, cookie, id string) (int, execAnswer, error) {
	client := &http.Client{Timeout: activityBudget, CheckRedirect: noRedirect}
	if cfg.DialContext != nil {
		client.Transport = jailTransport(cfg.DialContext)
	}
	return func(ctx context.Context, cookie, id string) (int, execAnswer, error) {
		var out execAnswer
		if !validHubID(id) {
			return 0, out, errBadUserID
		}
		reqBody, err := json.Marshal(struct {
			Command []string `json:"command"`
			Timeout int      `json:"timeout"`
		}{activityCommand, activityExecSeconds})
		if err != nil {
			return 0, out, err
		}
		u := cfg.Target.ResolveReference(&url.URL{Path: "/api/v1/agents/" + id + "/exec"})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(reqBody))
		if err != nil {
			return 0, out, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", sessionCookieName+"="+cookie)
		resp, err := client.Do(req)
		if err != nil {
			return 0, out, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, out, nil
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, activityMaxHub+1))
		if err != nil {
			return resp.StatusCode, out, err
		}
		if len(b) > activityMaxHub {
			return resp.StatusCode, out, errActivityTooBig
		}
		return resp.StatusCode, out, json.Unmarshal(b, &out)
	}
}

// cleanActivity reduces captured pane text to printable lines: escape
// sequences (CSI, OSC, DCS and the rest, 7- and 8-bit) and every other
// control character go, as do format characters (bidi, zero-width, tags) and
// line separators, which could make one line read as another. A tab stays.
// Then the trailing blanks of each line and the blank lines at the ends go,
// and the last lines that fit the limits are kept.
//
// tmux stores a C1 introducer the agent prints as a plain cell and writes it
// back out, so a sequence never outlives its line or activityEscapeRunes: an
// unterminated one would otherwise hide every later line.
func cleanActivity(s string) (string, bool) {
	var b strings.Builder
	b.Grow(min(len(s), activityMaxHub))
	const (
		text = iota
		escape
		csi
		str     // OSC, DCS, SOS, PM, APC: up to BEL or ST
		strEsc  // ESC inside a string
		escTail // intermediates of a two-character sequence
	)
	state, seq := text, 0
	for _, c := range s {
		if state != text {
			seq++
			if c == '\n' || seq > activityEscapeRunes {
				state = text
			}
		}
		switch state {
		case escape:
			switch {
			case c == '[':
				state = csi
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				state = str
			case c >= 0x20 && c <= 0x2f:
				state = escTail
			default:
				state = text
			}
			continue
		case escTail:
			if c < 0x20 || c > 0x2f {
				state = text
			}
			continue
		case csi:
			if c >= 0x40 && c <= 0x7e || c > 0x7e {
				state = text
			}
			continue
		case str:
			switch c {
			case 0x07, 0x9c:
				state = text
			case 0x1b:
				state = strEsc
			}
			continue
		case strEsc:
			state = str
			if c == '\\' {
				state = text
			}
			continue
		}
		switch {
		case c == 0x1b:
			state, seq = escape, 0
		case c == 0x9b:
			state, seq = csi, 0
		case c == 0x90 || c == 0x9d || c == 0x98 || c == 0x9e || c == 0x9f:
			state, seq = str, 0
		case c == '\n' || c == '\t':
			b.WriteRune(c)
		case c < 0x20 || c >= 0x7f && c <= 0x9f:
		case c == 0x2028 || c == 0x2029 || unicode.Is(unicode.Cf, c):
		default:
			b.WriteRune(c)
		}
	}
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	truncated := false
	if len(lines) > activityMaxLines {
		lines, truncated = lines[len(lines)-activityMaxLines:], true
	}
	for i, l := range lines {
		if utf8.RuneCountInString(l) > activityLineRunes {
			lines[i], truncated = string([]rune(l)[:activityLineRunes])+"…", true
		}
	}
	size := 0
	start := len(lines)
	for start > 0 && size+len(lines[start-1])+1 <= activityMaxBytes {
		size += len(lines[start-1]) + 1
		start--
	}
	if start > 0 {
		truncated = true
	}
	return strings.Join(lines[start:], "\n"), truncated
}
