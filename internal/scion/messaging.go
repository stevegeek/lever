package scion

import (
	"context"
	"strings"

	"github.com/stevegeek/lever/internal/termsafe"
)

type MsgOpts struct {
	To        string // "agent:<name>", "user:<name>", or a bare agent name
	Body      string
	Interrupt bool
	Project   string // nil/"" for user-addressed messages
}

// Event is a typed Scion agent event; the core relays it verbatim, so it is kept
// as a dynamic map. ID() is the dedup key the bridge uses.
type Event map[string]any

func (e Event) ID() string { id, _ := e["id"].(string); return id }

// WorkerReportedPrefix starts every event message lever relays. The hub builds
// a notification's message from fields the watched agent sets, so the text is
// the worker's, never lever's or the hub's statement.
const WorkerReportedPrefix = "worker-reported: "

const maxEventMessage = 1024

// WorkerReported returns a copy of e that is safe to show the manager as data:
// "message" sanitized to one line, bounded and marked with
// WorkerReportedPrefix, and "status" passed through StatusLabel. It must stay
// idempotent: the broker and lever-manager both apply it.
func WorkerReported(e Event) Event {
	out := make(Event, len(e))
	for k, v := range e {
		out[k] = v
	}
	if msg, ok := e["message"].(string); ok {
		msg = termsafe.Sanitize(strings.TrimPrefix(msg, WorkerReportedPrefix))
		if limit := maxEventMessage - len(WorkerReportedPrefix); len(msg) > limit {
			msg = strings.ToValidUTF8(msg[:limit-len("…")], "") + "…"
		}
		out["message"] = WorkerReportedPrefix + msg
	} else if _, has := e["message"]; has {
		out["message"] = WorkerReportedPrefix
	}
	if st, has := e["status"]; has {
		s, _ := st.(string)
		out["status"] = StatusLabel(s)
	}
	return out
}

// StatusLabel is a notification status when it is one the hub produces, ""
// when empty, else UNRECOGNISED. The hub derives it from fields the watched
// agent can post as any text.
func StatusLabel(status string) string {
	lower := strings.ToLower(status)
	switch {
	case status == "":
		return ""
	case status != strings.ToUpper(lower):
		// The hub upper-cases what it stores; any other casing is not its.
	case status == "DELETED", status == "DELIVERY_FAILED":
		return status
	case ActivityLabel(lower) == lower, PhaseLabel(lower) == lower:
		return status
	}
	return strings.ToUpper(LabelUnrecognised)
}

// Message sends one message through scion message.
//
// Flags go first and both agent-controlled positionals sit behind --: cobra
// parses interspersed flags, so a body of exactly -b would become the
// broadcast flag and fan the send out to every agent, defeating the broker's
// per-recipient deny.
func (c *Client) Message(ctx context.Context, o MsgOpts) error {
	args := []string{"message"}
	if o.Interrupt {
		args = append(args, "--interrupt")
	}
	args = append(args, projectFlag(o.Project)...)
	args = append(args, nonInteractive, "--", o.To, o.Body)
	_, err := c.run(ctx, "", args...)
	return err
}

// Inbox reads the caller's event inbox via scion notifications, which exists
// in agent CLI mode where messages does not. unread=false adds --all. project
// scopes to a worker ("" is the caller's own inbox). Listing is a read: a user
// token needs inbox:read on a hub that has that scope, and nothing more.
func (c *Client) Inbox(ctx context.Context, unread bool, project string) ([]Event, error) {
	args := []string{"notifications", "--json", nonInteractive}
	if !unread {
		args = append(args, "--all")
	}
	args = append(args, projectFlag(project)...)
	// runValue: stdout only, so stderr warnings cannot corrupt the JSON.
	out, err := c.runValue(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	// scion wraps results in {"items":[...]}; tolerate a bare array too.
	var wrapped struct {
		Items []Event `json:"items"`
	}
	if err := parseJSON(out, &wrapped); err == nil && wrapped.Items != nil {
		return wrapped.Items, nil
	}
	var arr []Event
	if err := parseJSON(out, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}
