package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// PathOperatorWorkerEvents is `lever worker watch`: a long poll for one
// worker's hub events, on the operator socket only.
const PathOperatorWorkerEvents = "/operator/worker/events"

const (
	MaxEventsWait = 30 * time.Second
	// eventsPollDefault spaces the hub reads of one long poll: each is a
	// scion process in the jail.
	eventsPollDefault = 3 * time.Second
	// maxEventsAnswer bounds the events of one answer; the cursor resumes
	// after the last one sent.
	maxEventsAnswer = 100
	// maxCursorLen and maxCursorIDs bound what a caller may send back.
	maxCursorLen = 8 << 10
	maxCursorIDs = 64
)

// EventKindCompletion marks a worker's completed status. It is a trigger to
// look at the worker's work, never the result: the status and the summary
// are what the worker reported about itself.
const (
	EventKindCompletion = "completion"
	EventKindState      = "state"
)

// WorkerEventsRequest is the body of POST PathOperatorWorkerEvents. An empty
// Cursor starts the watch now: the answer carries no events, only a cursor
// past every event the worker has so far.
type WorkerEventsRequest struct {
	Worker      string `json:"worker"`
	Cursor      string `json:"cursor"`
	WaitSeconds int    `json:"wait_seconds"`
}

// WorkerEvent is one hub event of the worker. Status is a status the hub
// produces or UNRECOGNISED; Message is the worker's own text, one line,
// bounded and marked with scion.WorkerReportedPrefix.
type WorkerEvent struct {
	ID      string    `json:"id"`
	Worker  string    `json:"worker"`
	Kind    string    `json:"kind"`
	Status  string    `json:"status"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
}

// WorkerEventsResponse answers a long poll. Gone means the worker's record
// is no more, or is a new one (a purge, a recycle): the watch is over.
type WorkerEventsResponse struct {
	Worker string        `json:"worker"`
	Events []WorkerEvent `json:"events"`
	Cursor string        `json:"cursor"`
	Gone   bool          `json:"gone"`
}

// eventsCursor is the watch position: the hub record's id, the newest event
// time sent, and the ids sent at that time.
type eventsCursor struct {
	Agent string    `json:"a"`
	At    time.Time `json:"t"`
	IDs   []string  `json:"i,omitempty"`
}

func (c eventsCursor) encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (eventsCursor, bool) {
	var c eventsCursor
	if len(s) > maxCursorLen {
		return c, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Agent == "" || len(c.IDs) > maxCursorIDs {
		return eventsCursor{}, false
	}
	return c, true
}

func (c eventsCursor) after(t time.Time, id string) bool {
	return t.After(c.At) || t.Equal(c.At) && !slices.Contains(c.IDs, id)
}

func (c *eventsCursor) advance(t time.Time, id string) {
	switch {
	case t.After(c.At):
		c.At, c.IDs = t, []string{id}
	case !t.Equal(c.At):
	case len(c.IDs) < maxCursorIDs:
		c.IDs = append(c.IDs, id)
	default:
		// More events at one instant than the cursor can name: skip the
		// rest of that instant rather than send them on every poll.
		c.At, c.IDs = c.At.Add(time.Nanosecond), nil
	}
}

// handleOperatorWorkerEvents reads the notification feed without
// acknowledging anything, so the manager's inbox is as it was. The feed is
// fleet-wide (the controller's), so only events the hub attributes to this
// worker's record id leave the broker, as for /msg/list.
func (b *Broker) handleOperatorWorkerEvents(w http.ResponseWriter, r *http.Request) {
	var req WorkerEventsRequest
	spec, ok := b.operatorWorker(w, r, "events", &req, func() string { return req.Worker })
	if !ok {
		return
	}
	var cur eventsCursor
	if req.Cursor != "" {
		if cur, ok = decodeCursor(req.Cursor); !ok {
			b.audit("worker", operatorActor, "deny", "events "+spec.Name+": bad cursor")
			refuse(w, ReasonBadRequest, "bad cursor", http.StatusBadRequest)
			return
		}
	}
	if !b.runtimeReady(w) {
		return
	}
	if b.resolveAgentID == nil {
		b.audit("worker", operatorActor, "error", "events: agent id resolver not wired")
		refuse(w, ReasonRuntimeUnavailable, "worker events unavailable", http.StatusBadGateway)
		return
	}
	ctx := r.Context()
	wait := min(time.Duration(max(req.WaitSeconds, 0))*time.Second, MaxEventsWait)
	deadline := time.Now().Add(wait)
	poll := b.eventsPoll
	if poll <= 0 {
		poll = eventsPollDefault
	}
	for {
		resp, err := b.workerEventsOnce(ctx, spec, req.Cursor == "", &cur)
		if err != nil {
			b.audit("worker", operatorActor, "error", "events "+spec.Name+": "+err.Error())
			refuse(w, ReasonRuntimeError, "runtime error: the hub's events of worker "+spec.Name+" could not be read", http.StatusBadGateway)
			return
		}
		if resp.Gone || len(resp.Events) > 0 || req.Cursor == "" || !time.Now().Add(poll).Before(deadline) {
			writeJSON(w, resp)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// workerEventsOnce with start answers no events, only a cursor past the feed
// as it is.
func (b *Broker) workerEventsOnce(ctx context.Context, spec WorkerSpec, start bool, cur *eventsCursor) (WorkerEventsResponse, error) {
	resp := WorkerEventsResponse{Worker: spec.Name, Events: []WorkerEvent{}}
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		return resp, err
	}
	if phase == "" {
		resp.Gone, resp.Cursor = true, cur.encode()
		return resp, nil
	}
	id, err := b.resolveAgentID(ctx, spec.Name)
	if err != nil || id == "" {
		return resp, fmt.Errorf("resolving the agent id: %s", errText(err))
	}
	if start {
		*cur = eventsCursor{Agent: id}
	} else if cur.Agent != id {
		resp.Gone, resp.Cursor = true, cur.encode()
		return resp, nil
	}
	events, err := b.runtime.Inbox(ctx, false, b.instanceProject)
	if err != nil {
		return resp, err
	}
	type dated struct {
		at time.Time
		e  scion.Event
	}
	var fresh []dated
	for _, e := range eventsForAgent(events, id) {
		at, ok := eventTime(e)
		if !ok || e.ID() == "" || !cur.after(at, e.ID()) {
			continue
		}
		fresh = append(fresh, dated{at, e})
	}
	slices.SortStableFunc(fresh, func(a, b dated) int { return a.at.Compare(b.at) })
	for _, d := range fresh {
		if !start && len(resp.Events) == maxEventsAnswer {
			break
		}
		cur.advance(d.at, d.e.ID())
		if !start {
			resp.Events = append(resp.Events, workerEventOf(spec.Name, d.at, d.e))
		}
	}
	resp.Cursor = cur.encode()
	return resp, nil
}

// eventTime is the hub's own creation time of the event, which the worker
// does not choose.
func eventTime(e scion.Event) (time.Time, bool) {
	s, _ := e["createdAt"].(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func workerEventOf(worker string, at time.Time, e scion.Event) WorkerEvent {
	marked := scion.WorkerReported(e)
	msg, _ := marked["message"].(string)
	if msg == "" {
		msg = scion.WorkerReportedPrefix
	}
	status, _ := marked["status"].(string)
	if status == "" {
		status = strings.ToUpper(scion.LabelUnrecognised)
	}
	kind := EventKindState
	if status == strings.ToUpper(scion.ActivityCompleted) {
		kind = EventKindCompletion
	}
	return WorkerEvent{ID: boundedLogin(e.ID()), Worker: worker, Kind: kind, Status: status,
		Message: msg, At: at, Source: WorkerActivitySource}
}
