package scion

import "context"

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

// Message sends one message through `scion message`.
//
// Flags go FIRST and the two positionals sit behind a `--` terminator, because
// both are agent-controlled. scion's message command takes single-token boolean
// flags — `-b`/`--broadcast` (every running agent in the project) and
// `-a`/`--all` (every project on the hub) — and cobra parses flags interspersed
// with positionals by default. A body of exactly `-b` would therefore bind to
// the broadcast flag instead of being the message, leaving the recipient as the
// only positional: the send would fan out to every sibling agent, defeating the
// worker-to-worker deny the broker applied on the recipient alone. `--` makes
// the body inert as a flag whatever it contains.
func (c *Client) Message(ctx context.Context, o MsgOpts) error {
	args := []string{"message"}
	if o.Interrupt {
		args = append(args, "--interrupt")
	}
	args = append(args, projectFlag(o.Project)...)
	args = append(args, "--", o.To, o.Body)
	_, err := c.run(ctx, "", args...)
	return err
}

// Inbox reads the caller's typed event inbox via `scion notifications` — the
// command available in agent CLI mode (SCION_CLI_MODE=agent), where `messages`
// is absent. Events carry id/agentId/status/message. unread=false adds --all
// (all vs unacknowledged). Requires Hub mode. project scopes to a worker
// ("" = the caller's own inbox).
func (c *Client) Inbox(ctx context.Context, unread bool, project string) ([]Event, error) {
	args := []string{"notifications", "--json"}
	if !unread {
		args = append(args, "--all")
	}
	args = append(args, projectFlag(project)...)
	out, err := c.run(ctx, "", args...)
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
