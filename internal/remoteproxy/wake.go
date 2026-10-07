package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The wake route: POST /lever/api/agents/<name>/wake. A login that may
// message a suspended worker (or, the operator, a stopped one) asks lever
// to resume it (spec: wake on message). The resume itself is the broker's, over its 0600 operator
// socket (Config.Wake), the same code the manager's resume verb runs.
//
// Order matters: method, browser provenance, the login's lists, the hub
// state, then the limiter, so a refused request never spends the agent's
// one wake a minute. Every refusal is a fixed reason word; the broker's own
// text never reaches the page or the audit line.

// Wake audit decisions.
const (
	DecisionWake       Decision = "remote-wake"
	DecisionDenyWake   Decision = "deny-wake"
	DecisionWakeResult Decision = "remote-wake-result" // the broker's late answer, one more line
)

// WakeError carries the broker's HTTP status (0: the broker was not reached).
type WakeError struct {
	Status int
	Err    error
}

func (e *WakeError) Error() string {
	if e.Status == 0 {
		return "wake: " + e.Err.Error()
	}
	return "wake: HTTP " + strconv.Itoa(e.Status) + ": " + e.Err.Error()
}

func (e *WakeError) Unwrap() error { return e.Err }

const (
	chatAgentsPrefix = chatAgentsPath + "/"
	chatWakeSuffix   = "/wake"
	wakeEvery        = 60 * time.Second
	// wakeAnswerWait is how long the route waits for the broker before it
	// answers 202 anyway: the page polls the list for "running".
	wakeAnswerWait = 5 * time.Second
	// wakeBudget bounds the whole resume, which outlives the request.
	wakeBudget = 5 * time.Minute
)

// agentNameRE is config's agent name rule: the only names a wake path takes.
var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// wakeTarget reports whether p is under /lever/api/agents/: name is the
// agent of a well-formed wake path, or "" for any other path there.
func wakeTarget(p string) (name string, under bool) {
	rest, ok := strings.CutPrefix(p, chatAgentsPrefix)
	if !ok {
		return "", false
	}
	if n, ok := strings.CutSuffix(rest, chatWakeSuffix); ok && agentNameRE.MatchString(n) {
		return n, true
	}
	return "", true
}

// wakeLimiter allows one wake per agent per wakeEvery, across all logins.
type wakeLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// take spends name's wake for wakeEvery, or reports how long until it may.
func (l *wakeLimiter) take(name string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if t, ok := l.last[name]; ok && now.Sub(t) < wakeEvery {
		return false, wakeEvery - now.Sub(t)
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	// Forget the old entries: the map holds at most the agents woken in the
	// last minute.
	maps.DeleteFunc(l.last, func(_ string, t time.Time) bool { return now.Sub(t) >= wakeEvery })
	l.last[name] = now
	return true, 0
}

func (g *gate) wakeWait() time.Duration {
	if g.wakeWaitFor > 0 {
		return g.wakeWaitFor
	}
	return wakeAnswerWait
}

func (g *gate) serveWake(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, name string) {
	refuse := func(status int, word string, extra map[string]string) {
		line.Reason = word
		body := map[string]string{"error": word}
		maps.Copy(body, extra)
		g.answerWakeJSON(w, r, line, DecisionDenyWake, status, body)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(http.StatusMethodNotAllowed, "method", nil)
		return
	}
	// The gate let a request with no Origin through (checkOrigin); a wake
	// is a browser POST, which always carries one, so one is required here,
	// and Sec-Fetch-Site, when sent, must be same-origin ("none" is a typed
	// navigation, never a page's fetch). The Origin must be the proxy's own
	// (sameOriginWrite).
	if !sameOriginWrite(r, g.cfg.ServeHost) {
		refuse(http.StatusForbidden, "origin", nil)
		return
	}
	// One answer for the manager, a see-only agent, a hidden agent and a
	// name that does not exist, so the answer tells none apart.
	if name == g.cfg.ChatAgent || !v.mayMessage(name) || !slices.Contains(g.cfg.Workers, name) {
		refuse(http.StatusForbidden, "not-allowed", nil)
		return
	}
	recs, err := g.records(r.Context())
	rec, found := recs[name]
	if found && !validHubID(rec.ID) {
		rec, found, err = AgentRecord{}, false, errBadUserID
	}
	state, _ := agentState(rec, found, err != nil)
	if v.tier == chatledger.TierContact && !contactFresh(state, name, g.cfg.ContactSession) {
		state = "not-fresh"
	}
	// A contact wakes only a suspended worker: stopped is the operator's
	// own decision. The operator may wake either.
	asleep := state == "suspended" || state == "stopped" && v.tier == chatledger.TierOperator
	if !asleep {
		refuse(http.StatusConflict, "not-asleep", map[string]string{"state": state})
		return
	}
	if g.cfg.Wake == nil {
		refuse(http.StatusServiceUnavailable, "unavailable", nil)
		return
	}
	if ok, wait := g.wakes.take(name); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		refuse(http.StatusTooManyRequests, "rate-limited", nil)
		return
	}
	// The resume outlives this request: a phone that drops the connection
	// must not cancel a resume half way.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), wakeBudget)
		defer cancel()
		done <- g.cfg.Wake(ctx, v.login, v.tier, name)
	}()
	timer := time.NewTimer(g.wakeWait())
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			status, word := wakeFailure(err)
			refuse(status, word, nil)
			return
		}
	case <-timer.C:
		go g.auditLateWake(*line, done)
	}
	g.answerWakeJSON(w, r, line, DecisionWake, http.StatusAccepted, map[string]string{"state": "starting"})
}

// wakeFailure maps a broker answer to the page's status and reason word: a
// broker refusal (4xx: not asleep, not a worker, the role guard, the hub
// refused) is "refused"; anything else is "failed".
func wakeFailure(err error) (int, string) {
	var we *WakeError
	if errors.As(err, &we) && we.Status >= 400 && we.Status < 500 {
		return http.StatusConflict, "refused"
	}
	return http.StatusBadGateway, "failed"
}

// auditLateWake records the broker's answer to a wake the page was already
// told is starting. Never the error text: the broker's audit has it.
func (g *gate) auditLateWake(line AuditLine, done <-chan error) {
	err := <-done
	line.Time = time.Now().UTC()
	line.Decision, line.Status, line.Reason = DecisionWakeResult, http.StatusOK, ""
	if err != nil {
		line.Status, line.Reason = wakeFailure(err)
		var we *WakeError
		if errors.As(err, &we) && we.Status != 0 {
			line.Status = we.Status
		}
	}
	g.audit(line)
}

// answerWakeJSON writes a wake answer: JSON, inert as a document, not stored.
func (g *gate) answerWakeJSON(w http.ResponseWriter, r *http.Request, line *AuditLine, decision Decision, status int, body map[string]string) {
	b, _ := json.Marshal(body)
	g.answerChat(w, line, decision, status, func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, append(b, '\n'), r)
}
