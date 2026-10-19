package remoteproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The new conversation route: POST /lever/api/new-conversation asks lever
// (Config.NewConversation) to back the manager's conversation up and start it
// again in a fresh session, and answers with the change's id; POST
// /lever/api/new-conversation/status with that id says how the change ended.
// The page follows the change by its result: the manager can stop and start
// again between two reads of the list. The operator's logins only: a contact
// never resets the manager. Refusals are fixed words; lever's text never
// reaches the page or the audit line.

// New conversation audit decisions.
const (
	DecisionNewConversation       Decision = "remote-new-conversation"
	DecisionDenyNewConversation   Decision = "deny-new-conversation"
	DecisionNewConversationResult Decision = "remote-new-conversation-result" // the late answer, one more line
	DecisionNewConversationStatus Decision = "remote-new-conversation-status" // a read of the run's state
)

// Config.NewConversation's refusals that changed nothing.
var (
	ErrConversationBusy   = errors.New("the manager is in the middle of a turn")
	ErrConversationLocked = errors.New("another change to the manager is running")
)

const (
	newConvPath       = "/lever/api/new-conversation"
	newConvStatusPath = newConvPath + "/status"
	// newConvAnswerWait is how long the route waits before it answers 202:
	// the refusals (busy, locked) come in seconds, the start takes a minute.
	newConvAnswerWait = 5 * time.Second
	// newConvBudget bounds the whole change, which outlives the request.
	newConvBudget  = 15 * time.Minute
	newConvBodyMax = 256
)

func (g *gate) newConvWait() time.Duration {
	if g.newConvWaitFor > 0 {
		return g.newConvWaitFor
	}
	return newConvAnswerWait
}

// newConvOp is the latest change the route started; its result stays until
// the next one.
type newConvOp struct {
	mu      sync.Mutex
	id      string
	running bool
	backup  string // "" when the home held no conversation to back up
	word    string // the failure's word; "" on success
}

func (o *newConvOp) busy() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.running
}

// begin starts a change, or reports false while one runs.
func (o *newConvOp) begin() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return "", false
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	o.id, o.running, o.backup, o.word = hex.EncodeToString(b[:]), true, "", ""
	return o.id, true
}

func (o *newConvOp) end(id, backup, word string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.id == id {
		o.running, o.backup, o.word = false, backup, word
	}
}

// status answers for id: running, ok (with the backup) or failed (with the
// word); found is false for an id the proxy does not hold.
func (o *newConvOp) status(id string) (body map[string]string, found bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if id == "" || subtle.ConstantTimeCompare([]byte(id), []byte(o.id)) != 1 {
		return nil, false
	}
	switch {
	case o.running:
		return map[string]string{"state": "running"}, true
	case o.word != "":
		return map[string]string{"state": "failed", "error": o.word}, true
	case o.backup != "":
		return map[string]string{"state": "ok", "backup": o.backup}, true
	}
	return map[string]string{"state": "ok"}, true
}

func (g *gate) serveNewConversation(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer) {
	refuse := func(status int, word string, extra map[string]string) {
		line.Reason = word
		body := map[string]string{"error": word}
		maps.Copy(body, extra)
		g.answerWakeJSON(w, r, line, DecisionDenyNewConversation, status, body)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(http.StatusMethodNotAllowed, "method", nil)
		return
	}
	// A browser POST from the page: its Origin is required (see serveWake).
	if !sameOriginWrite(r, g.cfg.ServeHost) {
		refuse(http.StatusForbidden, "origin", nil)
		return
	}
	if v.tier != chatledger.TierOperator {
		refuse(http.StatusForbidden, "not-allowed", nil)
		return
	}
	if g.cfg.NewConversation == nil {
		refuse(http.StatusServiceUnavailable, "unavailable", nil)
		return
	}
	if r.URL.Path == newConvStatusPath {
		g.serveNewConversationStatus(w, r, line, refuse)
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, newConvBodyMax+1))
	if err != nil || len(body) > newConvBodyMax || len(body) > 0 && json.Unmarshal(body, &req) != nil {
		refuse(http.StatusBadRequest, "bad-request", nil)
		return
	}
	recs, err := g.records(r.Context())
	rec, found := recs[g.cfg.ChatAgent]
	state, activity := agentState(rec, found, err != nil)
	if state != "running" && state != "suspended" {
		refuse(http.StatusConflict, "not-running", map[string]string{"state": state})
		return
	}
	// The command checks again: the activity can change before it looks.
	if activity == "working" && !req.Force {
		refuse(http.StatusConflict, "busy", nil)
		return
	}
	if g.newConv.busy() {
		refuse(http.StatusConflict, "in-progress", nil)
		return
	}
	if ok, wait := g.newConvs.take(g.cfg.ChatAgent); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		refuse(http.StatusTooManyRequests, "rate-limited", nil)
		return
	}
	id, ok := g.newConv.begin()
	if !ok {
		g.newConvs.give(g.cfg.ChatAgent)
		refuse(http.StatusConflict, "in-progress", nil)
		return
	}
	if req.Force {
		line.Reason = "force"
	}
	// The change outlives this request: a phone that drops the connection
	// must not stop it half way.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), newConvBudget)
		defer cancel()
		backup, err := g.cfg.NewConversation(ctx, req.Force)
		word := ""
		if err != nil {
			_, word = newConvFailure(err)
			backup = ""
		}
		g.newConv.end(id, backup, word)
		done <- err
	}()
	timer := time.NewTimer(g.newConvWait())
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			if errors.Is(err, ErrConversationBusy) || errors.Is(err, ErrConversationLocked) {
				g.newConvs.give(g.cfg.ChatAgent)
			}
			status, word := newConvFailure(err)
			refuse(status, word, nil)
			return
		}
	case <-timer.C:
		go g.auditLateNewConversation(*line, done)
	}
	g.answerWakeJSON(w, r, line, DecisionNewConversation, http.StatusAccepted, map[string]string{"state": "starting", "id": id})
}

// serveNewConversationStatus answers {"id"} with the change's state. The
// method, origin and tier are checked already.
func (g *gate) serveNewConversationStatus(w http.ResponseWriter, r *http.Request, line *AuditLine, refuse func(int, string, map[string]string)) {
	var req struct {
		ID string `json:"id"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, newConvBodyMax+1))
	if err != nil || len(body) > newConvBodyMax || json.Unmarshal(body, &req) != nil {
		refuse(http.StatusBadRequest, "bad-request", nil)
		return
	}
	ans, found := g.newConv.status(req.ID)
	if !found {
		line.Reason = "unknown"
		g.answerWakeJSON(w, r, line, DecisionNewConversationStatus, http.StatusNotFound, map[string]string{"error": "unknown"})
		return
	}
	line.Reason = ans["state"]
	g.answerWakeJSON(w, r, line, DecisionNewConversationStatus, http.StatusOK, ans)
}

func newConvFailure(err error) (int, string) {
	switch {
	case errors.Is(err, ErrConversationBusy):
		return http.StatusConflict, "busy"
	case errors.Is(err, ErrConversationLocked):
		return http.StatusConflict, "in-progress"
	case errors.Is(err, context.DeadlineExceeded):
		// The run goes on in its own session; the proxy stopped waiting.
		return http.StatusGatewayTimeout, "timeout"
	}
	return http.StatusBadGateway, "failed"
}

// auditLateNewConversation records the outcome of a change the page was
// already told is starting. Never the error text: lever's own log has it.
func (g *gate) auditLateNewConversation(line AuditLine, done <-chan error) {
	err := <-done
	line.Time = time.Now().UTC()
	line.Decision, line.Status, line.Reason = DecisionNewConversationResult, http.StatusOK, ""
	if err != nil {
		line.Status, line.Reason = newConvFailure(err)
	}
	g.audit(line)
}
