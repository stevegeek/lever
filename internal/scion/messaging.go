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

// ExcerptPrefix starts a notification's "excerpt": text another principal
// wrote (scion e8160a10 on; an artifact review's summary note), data like a
// worker's message.
const ExcerptPrefix = "excerpt-reported: "

// maxEventField bounds the other string fields of a notification. scion
// e8160a10 adds kind, resourceType and resourceId, and an agent can shape
// what they hold.
const maxEventField = 128

// WorkerReported returns a copy of e that is safe to show the manager as data:
// "message" sanitized to one line, bounded and marked with
// WorkerReportedPrefix, "excerpt" the same with ExcerptPrefix, "status"
// passed through StatusLabel, and every other string field sanitized and
// bounded. It must stay idempotent: the broker and lever-manager both apply
// it.
func WorkerReported(e Event) Event {
	out := make(Event, len(e))
	for k, v := range e {
		if str, ok := v.(string); ok {
			v = boundedText(termsafe.Sanitize(str), maxEventField)
		}
		out[k] = v
	}
	markText(e, out, "message", WorkerReportedPrefix)
	markText(e, out, "excerpt", ExcerptPrefix)
	if st, has := e["status"]; has {
		s, _ := st.(string)
		out["status"] = StatusLabel(s)
	}
	return out
}

// markText sets out[key] to e[key] sanitized to one line, bounded and marked
// with prefix, when e has the key.
func markText(e, out Event, key, prefix string) {
	if msg, ok := e[key].(string); ok {
		msg = termsafe.Sanitize(strings.TrimPrefix(msg, prefix))
		out[key] = prefix + boundedText(msg, maxEventMessage-len(prefix))
	} else if _, has := e[key]; has {
		out[key] = prefix
	}
}

// boundedText cuts s to at most limit bytes, ending a cut text with "…".
func boundedText(s string, limit int) string {
	if len(s) > limit {
		return strings.ToValidUTF8(s[:limit-len("…")], "") + "…"
	}
	return s
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
