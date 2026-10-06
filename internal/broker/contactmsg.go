package broker

// Messages an agent sends to a contact (remote.agent_messages).
//
// The hub lets an agent send to any hub user at any time, and stamps the
// agent as the sender. Lever cannot stop the send; it decides what a contact
// is SHOWN. So the broker authorizes a message here — the contact lists the
// caller, a reply answers that contact's verified post, an initiated message
// is inside the limit — and records the sha256 of its exact text in the
// agent ledger. The agent then sends that text itself, and the remote proxy
// shows a contact only agent messages whose text the ledger holds
// (/operator/agent-messages/match). The text is never logged.

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/agentledger"
	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/wire"
)

// AgentMessagesConfig is remote.agent_messages as the broker needs it.
type AgentMessagesConfig struct {
	Enabled       bool
	FollowUpAfter time.Duration
	MaxChars      int
	// LedgerDir is the agent ledger; "" = the state directory is inside
	// the tree, so nothing can be authorized (every call: unavailable).
	LedgerDir string
	Contacts  []ContactEntry
}

// ContactEntry is one tier: contact login: its hub email (lowercased, the
// address a send goes to) and the agents it lists to message.
type ContactEntry struct {
	Login, Email string
	Agents       []string
}

// Defaults for a config built without the effective values (brokerctl
// always passes them; config.DefaultAgentFollowUpAfter/DefaultAgentMaxChars).
const (
	defaultFollowUpAfter = 24 * time.Hour
	defaultMaxChars      = 4000
)

// withDefaults fills a zero FollowUpAfter or MaxChars, so a zero limit
// never allows a reminder at once or refuses every text.
func (c AgentMessagesConfig) withDefaults() AgentMessagesConfig {
	if c.FollowUpAfter <= 0 {
		c.FollowUpAfter = defaultFollowUpAfter
	}
	if c.MaxChars <= 0 {
		c.MaxChars = defaultMaxChars
	}
	return c
}

const (
	// contactCallLimit bounds contacts + contact_message calls per agent per
	// minute, so refusals cannot flood the audit log.
	contactCallLimit = 60
	// contactAuthPerHour bounds authorizations per agent per hour, counted
	// from the ledger (it survives a restart).
	contactAuthPerHour = 30
	// contactReplyWindow is how long after a contact's post an agent may
	// reply to it outside the initiate rule.
	contactReplyWindow = 24 * time.Hour
	// contactRepliesPerRef bounds the replies to one contact post, so one
	// verified post cannot carry an agent's whole hourly rate for a day.
	// Counted from the ledger, under its lock.
	contactRepliesPerRef = 3
	// contactBodyLimit fits a 16000-character text in JSON escapes.
	contactBodyLimit = 128 << 10
)

// The refusal words.
const (
	refuseNotContact  = "not-a-contact"
	refuseLimit       = "limit"
	refuseTooLong     = "too-long"
	refuseEmpty       = "empty"
	refuseBadRef      = "bad-ref"
	refuseRate        = "rate"
	refuseBadText     = "bad-text"
	refuseOff         = "off"
	refuseUnavailable = "unavailable"
)

// agentRecord opens the agent ledger on first use and again after a failure.
type agentRecord struct {
	dir    string
	mu     sync.Mutex
	ledger *agentledger.Ledger
}

func (a *agentRecord) get() (*agentledger.Ledger, error) {
	if a.dir == "" {
		return nil, errors.New("the agent ledger is off: the state directory is inside the tree")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ledger == nil {
		l, err := agentledger.Open(a.dir)
		if err != nil {
			return nil, err
		}
		a.ledger = l
	}
	return a.ledger, nil
}

// contactFor is the entry for login when it lists agent.
func (b *Broker) contactFor(login, agent string) (ContactEntry, bool) {
	for _, c := range b.agentMsgs.Contacts {
		if c.Login == login {
			for _, a := range c.Agents {
				if a == agent {
					return c, replyRef("user:"+c.Email) != ""
				}
			}
		}
	}
	return ContactEntry{}, false
}

// initiateDecision is the limit on messages an agent starts, per contact:
// with none unanswered it may start one; with one, a single reminder once
// followUp has passed; with two, nothing until the contact writes. initiated
// are the creation times of the agent's initiated authorizations to the
// contact; only those after the contact's last post count.
func initiateDecision(initiated []time.Time, lastFromContact time.Time, followUp time.Duration, now time.Time) (bool, time.Time) {
	var open []time.Time
	for _, t := range initiated {
		if t.After(lastFromContact) {
			open = append(open, t)
		}
	}
	switch len(open) {
	case 0:
		return true, time.Time{}
	case 1:
		next := open[0].Add(followUp)
		if !now.Before(next) {
			return true, time.Time{}
		}
		return false, next
	}
	return false, time.Time{}
}

// contactText checks the text the hub would store unchanged: valid UTF-8, no
// NUL or other control but tab and newline, no "@" at a word start (the hub
// rewrites "@<member email>" and routes agent @mentions), not blank, at most
// max characters as the hub counts them. U+FFFD is refused too: the JSON
// decoder puts it in place of invalid UTF-8, so it marks bytes that were not
// text before they reached the broker.
func contactText(text string, max int) string {
	if !utf8.ValidString(text) {
		return refuseBadText
	}
	prev := rune(-1) // the rune before r; -1 at the start
	for _, r := range text {
		if r == '＠' || r == '@' && (prev < 0 || !wordRune(prev)) {
			return refuseBadText
		}
		prev = r
		if r == utf8.RuneError || r != '\n' && r != '\t' && unicode.IsControl(r) {
			return refuseBadText
		}
	}
	if strings.TrimSpace(text) == "" {
		return refuseEmpty
	}
	if utf8.RuneCountInString(text) > max {
		return refuseTooLong
	}
	return ""
}

// wordRune is scion's mention boundary rule (mention_translate.go
// isWordChar) applied to the rune before an "@": an ASCII letter, digit,
// "_" or "-" there means the "@" is inside a word, which the hub leaves
// alone. Any other rune before it (space, punctuation, a non-ASCII letter
// such as "é") is a boundary to the hub, so the "@" there is refused.
func wordRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

func (b *Broker) handleContacts(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "contact", "")
	if !ok {
		return
	}
	now := time.Now()
	if ok, _ := b.contactRate.take(caller, now); !ok {
		writeJSON(w, wire.ContactsResponse{Enabled: b.agentMsgs.Enabled, Note: refuseRate})
		return
	}
	if !b.agentMsgs.Enabled {
		writeJSON(w, wire.ContactsResponse{Note: "agent messages are off on this instance: answer a contact only as your skill says"})
		return
	}
	_, slug, _, _ := b.identity(caller)
	led, lerr := b.agentLedger.get()
	agentID, aerr := b.agentHubID(r, slug)
	out := wire.ContactsResponse{Enabled: true, Contacts: []wire.ContactInfo{}}
	for _, c := range b.agentMsgs.Contacts {
		if _, ok := b.contactFor(c.Login, slug); !ok {
			continue
		}
		info := wire.ContactInfo{Login: c.Login, To: replyRef("user:" + c.Email)}
		if lerr == nil && aerr == nil && b.chatLedger != "" {
			last, err1 := chatledger.LastPost(b.chatLedger, c.Login, agentID)
			v, err2 := led.View(slug, c.Login, now)
			if err1 == nil && err2 == nil {
				var initiated []time.Time
				for _, a := range v.ForContact {
					if a.Kind == agentledger.KindInitiated {
						initiated = append(initiated, a.Created)
					}
				}
				can, next := initiateDecision(initiated, last, b.agentMsgs.FollowUpAfter, now)
				info.CanInitiate = can
				info.LastFromContact, info.NextAllowedAt = rfc(last), rfc(next)
				if n := len(initiated); n > 0 {
					info.LastInitiated = rfc(initiated[n-1])
				}
			}
		}
		out.Contacts = append(out.Contacts, info)
	}
	b.audit("contact", caller, "allow", "contacts", "count", len(out.Contacts))
	writeJSON(w, out)
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// agentHubID is the caller's hub agent id (resolveAgentID, as verifyWeb).
func (b *Broker) agentHubID(r *http.Request, slug string) (string, error) {
	if b.resolveAgentID == nil {
		return "", errors.New("no agent id resolver")
	}
	id, err := b.resolveAgentID(r.Context(), slug)
	if err == nil && id == "" {
		err = errors.New("no agent id")
	}
	return id, err
}

var errRate = errors.New(refuseRate)

// errReplyCap: the contact post already has contactRepliesPerRef replies.
var errReplyCap = errors.New(refuseLimit)

type limitErr struct{ next time.Time }

func (e limitErr) Error() string { return refuseLimit }

func (b *Broker) handleContactMessage(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "contact", "")
	if !ok {
		return
	}
	now := time.Now()
	var req wire.ContactMessageRequest
	refuse := func(word, note, detail string, next time.Time) {
		b.audit("contact", caller, "deny", "message to="+boundedLogin(req.To)+": "+detail, "reason", word)
		writeJSON(w, wire.ContactMessageResponse{Reason: word, Note: note, NextAllowedAt: rfc(next)})
	}
	if ok, _ := b.contactRate.take(caller, now); !ok {
		refuse(refuseRate, "too many calls this minute", "call rate", time.Time{})
		return
	}
	if err := decodeBody(w, r, contactBodyLimit, &req); err != nil {
		refuse(refuseBadText, "the request was not valid JSON", "bad body", time.Time{})
		return
	}
	if !b.agentMsgs.Enabled {
		refuse(refuseOff, "agent messages are off on this instance", "off", time.Time{})
		return
	}
	_, slug, _, _ := b.identity(caller)
	c, ok := b.contactFor(req.To, slug)
	if !ok {
		refuse(refuseNotContact, "to must be a login from contacts()", "not a contact of "+slug, time.Time{})
		return
	}
	if word := contactText(req.Text, b.agentMsgs.MaxChars); word != "" {
		refuse(word, "see the skill's text rules", "text", time.Time{})
		return
	}
	led, err := b.agentLedger.get()
	if err != nil || b.chatLedger == "" {
		refuse(refuseUnavailable, "the host record is unavailable", "ledger: "+errText(err), time.Time{})
		return
	}
	agentID, err := b.agentHubID(r, slug)
	if err != nil {
		refuse(refuseUnavailable, "the broker cannot resolve your hub agent id", "agent id: "+err.Error(), time.Time{})
		return
	}
	kind, replyTo := agentledger.KindInitiated, ""
	var last time.Time
	if ref := strings.TrimSpace(req.ReplyToRef); ref != "" {
		e, found, err := chatledger.ByMessageID(b.chatLedger, c.Login, ref)
		if err != nil || !found || e.Tier != chatledger.TierContact || e.AgentID != agentID ||
			now.Sub(e.Recorded) > contactReplyWindow || !b.chatUses.verifiedBy(caller, ref, now) {
			refuse(refuseBadRef, "reply_to_ref must be the message_id message_verify returned for this contact's post to you (within 24h)", "bad ref", time.Time{})
			return
		}
		kind, replyTo = agentledger.KindReply, ref
	} else if last, err = chatledger.LastPost(b.chatLedger, c.Login, agentID); err != nil {
		refuse(refuseUnavailable, "the chat ledger cannot be read", "chat ledger: "+err.Error(), time.Time{})
		return
	}
	id, err := agentledger.NewID()
	if err != nil {
		refuse(refuseUnavailable, "the host record is unavailable", "id: "+err.Error(), time.Time{})
		return
	}
	a := agentledger.Auth{ID: id, Agent: slug, Contact: c.Login, Kind: kind, SHA256: agentledger.HashText(req.Text),
		Length: utf8.RuneCountInString(req.Text), ReplyTo: replyTo, Created: now, Expires: now.Add(agentledger.TTL)}
	err = led.Authorize(a, now, func(v agentledger.View) error {
		if v.LastHour >= contactAuthPerHour {
			return errRate
		}
		if kind == agentledger.KindReply {
			n := 0
			for _, p := range v.ForContact {
				if p.Kind == agentledger.KindReply && p.ReplyTo == replyTo {
					n++
				}
			}
			if n >= contactRepliesPerRef {
				return errReplyCap
			}
			return nil
		}
		var initiated []time.Time
		for _, p := range v.ForContact {
			if p.Kind == agentledger.KindInitiated {
				initiated = append(initiated, p.Created)
			}
		}
		if ok, next := initiateDecision(initiated, last, b.agentMsgs.FollowUpAfter, now); !ok {
			return limitErr{next}
		}
		return nil
	})
	var le limitErr
	switch {
	case errors.As(err, &le):
		refuse(refuseLimit, "you have an unanswered message to this contact; wait for their answer or next_allowed_at", "limit", le.next)
		return
	case errors.Is(err, errReplyCap):
		refuse(refuseLimit, "this contact message already has 3 replies; wait for the contact to write again", "reply cap", time.Time{})
		return
	case errors.Is(err, errRate):
		refuse(refuseRate, "too many messages this hour", "hourly rate", time.Time{})
		return
	case err != nil:
		refuse(refuseUnavailable, "the host record is unavailable", "authorize: "+err.Error(), time.Time{})
		return
	}
	b.audit("contact", caller, "allow", "message to="+boundedLogin(c.Login)+" kind="+kind, "ref", id, "length", a.Length)
	writeJSON(w, wire.ContactMessageResponse{OK: true, Ref: id, Kind: kind, To: replyRef("user:" + c.Email), Expires: rfc(a.Expires)})
}
