package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// contacts and contact_message (remote.agent_messages): a message to a
// contact goes through the broker first (authorize and record), then the
// agent sends the recorded text with scion message. contactMessageTool
// writes that text to a 0600 file itself, so the bytes the hub stores are
// the bytes the broker hashed: no editor newline, no shell quoting.

const contactsDescription = "List the contacts you may message (the logins whose lever config lists you), " +
	"with when they last wrote to you and whether you may start a message now (can_initiate, next_allowed_at)."

const contactMessageDescription = "Authorize one message to a contact; lever records it and writes its exact text to body_file. " +
	"Then run the returned command once, unchanged. A message sent any other way is not shown to the contact. " +
	"Refusals: not-a-contact, limit, too-long, empty, bad-ref, bad-text, rate, off, unavailable."

var (
	// plainTarget is a scion reference safe inside the single quotes of the
	// returned command: "@" and a plain email.
	plainTarget = regexp.MustCompile(`^@[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+$`)
	// plainRef is a record id (agentledger.NewID).
	plainRef = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// plainPath is a body file path the command line carries unquoted.
	plainPath = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)
)

// contactToolSchemas are the tools/list entries of contacts and
// contact_message: plain top-level objects (no combinator, #24).
func contactToolSchemas(strProp func(string) map[string]any) []any {
	return []any{
		map[string]any{"name": "contacts", "description": contactsDescription,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
		map[string]any{"name": "contact_message", "description": contactMessageDescription,
			"inputSchema": map[string]any{"type": "object", "required": []string{"to", "text"},
				"properties": map[string]any{
					"to":           strProp("a login from contacts(), exactly as listed"),
					"text":         strProp("the whole message, exactly as it will be sent; no @ at the start of a word"),
					"reply_to_ref": strProp("for a reply: the message_id message_verify returned for the contact's post"),
				}}},
	}
}

// contactsTool returns the broker's answer to PathContacts unchanged.
func contactsTool(s *MCPServer, ctx context.Context, _ map[string]string) (string, error) {
	var raw json.RawMessage
	if err := httpjson.Post(ctx, s.client, s.brokerURL+wire.PathContacts, struct{}{}, &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// contactMessageTool authorizes the text with the broker and, when allowed,
// writes it to a new 0600 body file and returns the one command that sends
// it. A refusal is a normal result (ok false, the reason word), so the model
// reads the word; a broker answer this tool would not put on a command line
// is an error.
func contactMessageTool(s *MCPServer, ctx context.Context, args map[string]string) (string, error) {
	// to goes to the broker as given: it compares it exactly with the
	// configured logins, so " c@x" is not-a-contact rather than a guess.
	to, text, ref := args["to"], args["text"], strings.TrimSpace(args["reply_to_ref"])
	if to == "" || text == "" {
		return "", invalidParams{errors.New(`"to" and "text" are required`)}
	}
	var resp wire.ContactMessageResponse
	if err := httpjson.Post(ctx, s.client, s.brokerURL+wire.PathContactMessage,
		wire.ContactMessageRequest{To: to, Text: text, ReplyToRef: ref}, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		out := map[string]any{"ok": false, "reason": resp.Reason, "note": resp.Note}
		if resp.NextAllowedAt != "" {
			out["next_allowed_at"] = resp.NextAllowedAt
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	if !plainTarget.MatchString(resp.To) || !plainRef.MatchString(resp.Ref) {
		return "", errors.New("the broker named a target or ref this tool will not put on a command line")
	}
	path := filepath.Join(s.bodyDir, "lever-contact-"+resp.Ref+".txt")
	if !plainPath.MatchString(path) {
		return "", fmt.Errorf("the body directory %q is not a plain path this tool will put on a command line", s.bodyDir)
	}
	// O_EXCL + O_NOFOLLOW: never write through a file or link that is
	// already there.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("writing the body file: %w", err)
	}
	// The exact authorized bytes, trailing newline included: scion message
	// --body-file reads the file unchanged (cmd/message.go
	// resolveMessageBody; only the stdin form "-" trims), and the hub
	// stores it unchanged once the broker refused mention-like "@".
	_, werr := f.WriteString(text)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("writing the body file: %w", err)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "ref": resp.Ref, "kind": resp.Kind, "to": resp.To, "expires": resp.Expires,
		"body_file": path, "command": "scion message --body-file " + path + " -- '" + resp.To + "'"})
	return string(b), nil
}
