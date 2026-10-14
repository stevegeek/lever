package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The new conversation route: POST /lever/api/new-conversation asks lever
// (Config.NewConversation) to back the manager's conversation up and start it
// again in a fresh session. The operator's logins only: a contact never
// resets the manager. Refusals are fixed words; lever's text never reaches
// the page or the audit line.

// New conversation audit decisions.
const (
	DecisionNewConversation       Decision = "remote-new-conversation"
	DecisionDenyNewConversation   Decision = "deny-new-conversation"
	DecisionNewConversationResult Decision = "remote-new-conversation-result" // the late answer, one more line
)

// Config.NewConversation's refusals that changed nothing.
var (
	ErrConversationBusy   = errors.New("the manager is in the middle of a turn")
	ErrConversationLocked = errors.New("another conversation change is running")
)

const (
	newConvPath = "/lever/api/new-conversation"
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
	if ok, wait := g.newConvs.take(g.cfg.ChatAgent); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		refuse(http.StatusTooManyRequests, "rate-limited", nil)
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
		done <- g.cfg.NewConversation(ctx, req.Force)
	}()
	timer := time.NewTimer(g.newConvWait())
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			status, word := newConvFailure(err)
			refuse(status, word, nil)
			return
		}
	case <-timer.C:
		go g.auditLateNewConversation(*line, done)
	}
	g.answerWakeJSON(w, r, line, DecisionNewConversation, http.StatusAccepted, map[string]string{"state": "starting"})
}

func newConvFailure(err error) (int, string) {
	switch {
	case errors.Is(err, ErrConversationBusy):
		return http.StatusConflict, "busy"
	case errors.Is(err, ErrConversationLocked):
		return http.StatusConflict, "in-progress"
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
