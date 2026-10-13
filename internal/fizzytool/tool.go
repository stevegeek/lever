package fizzytool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrOtherBoard: the card is not on the configured board.
var ErrOtherBoard = errors.New("card is not on the Lever board")

// DefaultWriteLimit is how many comments, created cards and closed cards
// together the tool sends in any one minute when Tool.WriteLimit is 0.
const DefaultWriteLimit = 10

// Tool is the six operations of spec §4b.3, and the opt-in close_card.
type Tool struct {
	CLI    CLI
	Board  string
	Prefix string // prepended to every comment and description
	TmpDir string // private (0700) dir for body files
	Log    *slog.Logger
	// WriteLimit caps comments, created cards and closed cards per sliding
	// minute for this process, so a looping agent cannot flood the board
	// (and its watchers' notifications). 0 means DefaultWriteLimit.
	WriteLimit int
	// Now is time.Now; a test seam.
	Now func() time.Time

	mu     sync.Mutex
	writes []time.Time // send times within the last minute, oldest first
}

// takeWrite counts one comment, card create or card close against the
// per-minute cap, or refuses it.
func (t *Tool) takeWrite() error { return t.takeWrites(1) }

// takeWrites counts n writes of one call against the cap together, or
// refuses them all, so a call is never cut off between its writes.
func (t *Tool) takeWrites(n int) error {
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	limit := t.WriteLimit
	if limit <= 0 {
		limit = DefaultWriteLimit
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	at := now()
	keep := t.writes[:0]
	for _, w := range t.writes {
		if at.Sub(w) < time.Minute {
			keep = append(keep, w)
		}
	}
	t.writes = keep
	if len(t.writes)+n > limit {
		return fmt.Errorf("rate: at most %d comments, new cards and closed cards per minute; try again in a minute", limit)
	}
	for range n {
		t.writes = append(t.writes, at)
	}
	return nil
}

func (t *Tool) audit(op, caller, card, column string, err error) {
	decision, detail := "allow", ""
	if err != nil {
		decision, detail = "deny", err.Error()
	}
	t.Log.Info("fizzy."+op, "caller", caller, "card", card, "column", column, "decision", decision, "detail", detail)
}

// checkCard returns the card's data after proving it is on t.Board.
func (t *Tool) checkCard(ctx context.Context, number string) (json.RawMessage, error) {
	data, err := t.CLI.Run(ctx, "card", "show", number)
	if err != nil {
		return nil, err
	}
	var c struct {
		Board struct {
			ID string `json:"id"`
		} `json:"board"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("card %s: unreadable response", number)
	}
	if c.Board.ID != t.Board {
		return nil, fmt.Errorf("card %s: %w", number, ErrOtherBoard)
	}
	return data, nil
}

// withTextFile writes text to a 0600 file in TmpDir for the duration of fn.
func (t *Tool) withTextFile(text string, fn func(path string) error) error {
	f, err := os.CreateTemp(t.TmpDir, "text-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return fn(f.Name())
}

func (t *Tool) ListCards(ctx context.Context, caller, column, search, page string) (res any, err error) {
	defer func() { t.audit("list_cards", caller, "", column, err) }()
	args := []string{"card", "list", "--board=" + t.Board}
	if page != "" {
		if err := ValidNumber(page); err != nil {
			return nil, fmt.Errorf("page: %w", err)
		}
		args = append(args, "--page="+page)
	}
	if column != "" {
		if err := ValidColumn(column, true); err != nil {
			return nil, err
		}
		args = append(args, "--column="+column)
	}
	if search != "" {
		if err := ValidSearch(search); err != nil {
			return nil, err
		}
		args = append(args, "--search="+search)
	}
	data, err := t.CLI.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	// The CLI may return cards of other boards; keep only this board's.
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("card list: unreadable response")
	}
	kept := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var c struct {
			Board struct {
				ID string `json:"id"`
			} `json:"board"`
		}
		if json.Unmarshal(it, &c) == nil && c.Board.ID == t.Board {
			kept = append(kept, it)
		}
	}
	out, err := json.Marshal(kept)
	return json.RawMessage(out), err
}

func (t *Tool) ShowCard(ctx context.Context, caller, number string) (res any, err error) {
	defer func() { t.audit("show_card", caller, number, "", err) }()
	if err := ValidNumber(number); err != nil {
		return nil, err
	}
	return t.checkCard(ctx, number)
}

func (t *Tool) ListComments(ctx context.Context, caller, number string) (res any, err error) {
	defer func() { t.audit("list_comments", caller, number, "", err) }()
	if err := ValidNumber(number); err != nil {
		return nil, err
	}
	if _, err := t.checkCard(ctx, number); err != nil {
		return nil, err
	}
	return t.CLI.Run(ctx, "comment", "list", "--card="+number)
}

func (t *Tool) Comment(ctx context.Context, caller, number, body string) (res any, err error) {
	defer func() { t.audit("comment", caller, number, "", err) }()
	if err := ValidNumber(number); err != nil {
		return nil, err
	}
	if err := ValidText(body); err != nil {
		return nil, err
	}
	if _, err := t.checkCard(ctx, number); err != nil {
		return nil, err
	}
	if err := t.takeWrite(); err != nil {
		return nil, err
	}
	var out json.RawMessage
	err = t.withTextFile(t.Prefix+body, func(p string) error {
		var e error
		out, e = t.CLI.Run(ctx, "comment", "create", "--card="+number, "--body_file="+p)
		return e
	})
	return out, err
}

func (t *Tool) MoveCard(ctx context.Context, caller, number, column string) (res any, err error) {
	defer func() { t.audit("move_card", caller, number, column, err) }()
	if err := ValidNumber(number); err != nil {
		return nil, err
	}
	if err := ValidColumn(column, false); err != nil {
		return nil, err
	}
	if _, err := t.checkCard(ctx, number); err != nil {
		return nil, err
	}
	cols, err := t.CLI.Run(ctx, "column", "list", "--board="+t.Board)
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(cols, &list); err != nil {
		return nil, fmt.Errorf("column list: unreadable response")
	}
	found := false
	var opts []string
	for _, c := range list {
		found = found || c.ID == column
		o := c.ID
		if c.Name != "" {
			o += " (" + c.Name + ")"
		}
		opts = append(opts, o)
	}
	if !found {
		return nil, fmt.Errorf("column %s is not a column of the board; columns: %s", column, strings.Join(opts, ", "))
	}
	return t.CLI.Run(ctx, "card", "column", number, "--column="+column)
}

// noDescription is the description CreateCard writes after the prefix when
// the caller gives none.
const noDescription = "(no description)"

func (t *Tool) CreateCard(ctx context.Context, caller, title, description string) (res any, err error) {
	defer func() { t.audit("create_card", caller, "", "", err) }()
	if err := ValidTitle(title); err != nil {
		return nil, err
	}
	if err := ValidText(description); err != nil {
		return nil, err
	}
	if err := t.takeWrite(); err != nil {
		return nil, err
	}
	// An empty description would leave the card with the bare prefix,
	// which reads like a truncated text.
	if strings.TrimSpace(description) == "" {
		description = noDescription
	}
	var out json.RawMessage
	err = t.withTextFile(t.Prefix+description, func(p string) error {
		var e error
		out, e = t.CLI.Run(ctx, "card", "create", "--board="+t.Board, "--title="+title, "--description_file="+p)
		return e
	})
	return out, err
}

// ErrClosed: the card is already closed.
var ErrClosed = errors.New("card is already closed")

// CloseCard closes an open card on t.Board (Fizzy's Done is the closed
// state, not a column), after posting comment with the prefix when one is
// given. A closed card is refused and nothing is posted. It never reopens
// or deletes a card.
func (t *Tool) CloseCard(ctx context.Context, caller, number, comment string) (res any, err error) {
	defer func() { t.audit("close_card", caller, number, "", err) }()
	if err := ValidNumber(number); err != nil {
		return nil, err
	}
	if err := ValidText(comment); err != nil {
		return nil, err
	}
	data, err := t.checkCard(ctx, number)
	if err != nil {
		return nil, err
	}
	// A card whose state cannot be read is refused, not closed blind.
	var c struct {
		Closed *bool `json:"closed"`
	}
	if json.Unmarshal(data, &c) != nil || c.Closed == nil {
		return nil, fmt.Errorf("card %s: response has no closed state", number)
	}
	if *c.Closed {
		return nil, fmt.Errorf("card %s: %w; nothing posted", number, ErrClosed)
	}
	withComment := strings.TrimSpace(comment) != ""
	writes := 1
	if withComment {
		writes = 2
	}
	if err := t.takeWrites(writes); err != nil {
		return nil, err
	}
	if withComment {
		err := t.withTextFile(t.Prefix+comment, func(p string) error {
			_, e := t.CLI.Run(ctx, "comment", "create", "--card="+number, "--body_file="+p)
			return e
		})
		if err != nil {
			return nil, fmt.Errorf("%w; the card was not closed", err)
		}
	}
	if _, err := t.CLI.Run(ctx, "card", "close", number); err != nil {
		if withComment {
			return nil, fmt.Errorf("%w; the comment was posted, and the card may still be open", err)
		}
		return nil, err
	}
	return map[string]any{"ok": true, "number": number, "closed": true, "commented": withComment}, nil
}
