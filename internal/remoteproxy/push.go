package remoteproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/stevegeek/lever/internal/webpush"
)

// Web Push for the chat page (remote.push).
//
// A login turns notifications on from the page: the browser subscribes with
// the VAPID public key and the page posts the subscription here. For each
// login with a subscription the proxy keeps one hub events stream with that
// login's own session (push_watch.go); a new message from one of the
// login's agents becomes one push of {"v":1,"agent":"<name>"} to each of
// its subscriptions (push_trigger.go). No message text, label or id is ever
// in a push. The page and its worker (chatui/sw.js) do the rest.

const (
	pushKeyPath  = "/lever/api/push/key"
	pushSubsPath = "/lever/api/push/subscriptions"
	chatSWPath   = "/lever/sw.js"
	maxSubBody   = 4 << 10

	// pushWritesPerMinute bounds one login's subscribe and unsubscribe
	// calls: each rewrites the store file.
	pushWritesPerMinute = 10
)

// Push audit decisions. No line carries an endpoint path, a key or text.
const (
	DecisionPushSubscribe   Decision = "push-subscribe"
	DecisionPushUnsubscribe Decision = "push-unsubscribe"
	DecisionDenyPush        Decision = "deny-push"
	DecisionPushSent        Decision = "push-sent"
	DecisionPushGone        Decision = "push-gone"
	DecisionPushFailed      Decision = "push-failed"
	DecisionPushStream      Decision = "push-stream"
)

type pushSender interface {
	Send(ctx context.Context, sub webpush.Subscription, payload []byte) error
}

type PushOptions struct {
	Dir       string // state.State.PushDir()
	Subject   string // remote.push.subject
	TestHosts webpush.TestHosts
	Logins    []string // allowed_users: the store forgets the rest
	Audit     func(AuditLine)
	Sender    pushSender // nil: a webpush.Sender with this key
}

// Push is the proxy's Web Push service.
type Push struct {
	store  *PushStore
	send   pushSender
	pub    string
	dir    string
	test   webpush.TestHosts
	audit  func(AuditLine)
	now    func() time.Time
	kicks  chan struct{}
	g      *gate
	writes *loginRate // subscribe and unsubscribe, per login

	mu   sync.Mutex
	last map[string]time.Time // login\x00agent → last push (push_trigger.go)

	// stopping closes when Run stops: outstanding sends get shutdownGrace
	// more (push_trigger.go).
	stopping chan struct{}
	stopOnce sync.Once

	// Watcher pacing (push_watch.go); tests shorten them.
	backoffMin, backoffMax, idle, healthy, debounce, shutdownGrace time.Duration
}

// NewPush opens the push directory, the key (created on first use) and
// the store.
func NewPush(o PushOptions) (*Push, error) {
	store, err := OpenPushStore(o.Dir, o.Logins)
	if err != nil {
		return nil, err
	}
	key, err := webpush.LoadOrCreateKey(filepath.Join(o.Dir, pushKeyFile))
	if err != nil {
		return nil, err
	}
	pub, err := webpush.PublicKey(key)
	if err != nil {
		return nil, err
	}
	send := o.Sender
	if send == nil {
		send = &webpush.Sender{Key: key, Subject: o.Subject, Test: o.TestHosts, Client: webpush.NewClient(o.TestHosts)}
	}
	p := &Push{store: store, send: send, pub: pub, dir: o.Dir, test: o.TestHosts, audit: o.Audit, now: time.Now,
		kicks: make(chan struct{}, 1), last: map[string]time.Time{}, stopping: make(chan struct{}), writes: newLoginRate(pushWritesPerMinute, time.Minute),
		backoffMin: time.Second, backoffMax: 5 * time.Minute, idle: 90 * time.Second, healthy: 2 * time.Minute, debounce: 2 * time.Second,
		shutdownGrace: 5 * time.Second}
	_ = WritePushStatus(o.Dir, PushStatus{At: p.now().UTC(), Result: "started", TestHosts: len(o.TestHosts) > 0})
	return p, nil
}

// PublicKey is the VAPID public key the page subscribes with.
func (p *Push) PublicKey() string { return p.pub }

func (p *Push) attach(g *gate) { p.g = g }

// stop starts the shutdown cap on outstanding sends.
func (p *Push) stop() {
	p.stopOnce.Do(func() {
		if p.stopping != nil {
			close(p.stopping)
		}
	})
}

// kick asks Run to start or stop watchers for the store's logins.
func (p *Push) kick() {
	select {
	case p.kicks <- struct{}{}:
	default:
	}
}

func (p *Push) subscribe(login string, sub webpush.Subscription) error {
	if err := p.store.Add(login, sub); err != nil {
		return err
	}
	p.kick()
	return nil
}

func (p *Push) unsubscribe(login, endpoint string) error {
	if _, err := p.store.Remove(login, endpoint); err != nil {
		return err
	}
	p.kick()
	return nil
}

// sameOriginWrite reports whether a write comes from the page itself: one
// Origin (the gate already refused another host, and "null" here), and a
// Sec-Fetch-Site, when sent, of same-origin. A browser always sends Origin
// on a POST or DELETE.
func sameOriginWrite(r *http.Request) bool {
	if o := r.Header.Values("Origin"); len(o) != 1 || o[0] == "" || o[0] == "null" {
		return false
	}
	s := r.Header.Values("Sec-Fetch-Site")
	return len(s) == 0 || len(s) == 1 && strings.EqualFold(s[0], "same-origin")
}

// servePush answers the key and subscription routes for v.
func (g *gate) servePush(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, p string) {
	refuse := func(status int, word string) {
		line.Reason = word
		g.answerWakeJSON(w, r, line, DecisionDenyPush, status, map[string]string{"error": word})
	}
	if p == pushKeyPath {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			refuse(http.StatusMethodNotAllowed, "method")
			return
		}
		g.answerWakeJSON(w, r, line, DecisionAllow, http.StatusOK, map[string]string{"key": g.push.pub})
		return
	}
	decision := DecisionPushSubscribe
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		decision = DecisionPushUnsubscribe
	default:
		w.Header().Set("Allow", "POST, DELETE")
		refuse(http.StatusMethodNotAllowed, "method")
		return
	}
	if !sameOriginWrite(r) {
		refuse(http.StatusForbidden, "origin")
		return
	}
	// JSON only: a cross-site form cannot send it without a preflight.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		refuse(http.StatusUnsupportedMediaType, "content-type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSubBody+1))
	if err != nil || len(body) > maxSubBody {
		refuse(http.StatusRequestEntityTooLarge, "too-large")
		return
	}
	var in pushSubBody
	if decodePushSubBody(body, &in) != nil {
		refuse(http.StatusBadRequest, "bad-json")
		return
	}
	// Counted only for a write that would reach the store.
	rated := func() bool {
		if g.push.writes.take(v.login, g.push.now()) {
			return true
		}
		w.Header().Set("Retry-After", "60")
		refuse(http.StatusTooManyRequests, "rate")
		return false
	}
	if r.Method == http.MethodDelete {
		if !rated() {
			return
		}
		if err := g.push.unsubscribe(v.login, in.Endpoint); err != nil {
			refuse(http.StatusServiceUnavailable, "unavailable")
			return
		}
		g.answerWakeJSON(w, r, line, decision, http.StatusOK, map[string]string{"ok": "true"})
		return
	}
	sub, err := webpush.ParseSubscription(in.Endpoint, in.Keys.P256DH, in.Keys.Auth, g.push.test)
	switch {
	case errors.Is(err, webpush.ErrEndpoint):
		refuse(http.StatusBadRequest, "endpoint")
		return
	case err != nil:
		refuse(http.StatusBadRequest, "keys")
		return
	}
	if !rated() {
		return
	}
	if err := g.push.subscribe(v.login, sub); err != nil {
		refuse(http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerWakeJSON(w, r, line, decision, http.StatusCreated, map[string]string{"ok": "true"})
}

// pushSubBody is what the page posts: PushSubscription.toJSON()'s fields, no
// others. expirationTime is accepted and ignored (browsers send null).
type pushSubBody struct {
	Endpoint       string          `json:"endpoint"`
	ExpirationTime json.RawMessage `json:"expirationTime"`
	Keys           struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// decodePushSubBody reads body strictly: one JSON object, no unknown field, no
// key twice in any object, nothing after it. Two readers of one body must
// never see two different subscriptions.
func decodePushSubBody(body []byte, out *pushSubBody) error {
	if err := noDuplicateKeys(body); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("push: data after the subscription")
	}
	return nil
}

// noDuplicateKeys walks body's tokens and refuses an object that names a
// key twice (encoding/json would keep the last one silently). Keys compare
// case-folded, as encoding/json matches them to fields: "endpoint" and
// "ENDPOINT" (or "keys" and "keyſ") are the same field.
func noDuplicateKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		object, wantKey bool
		keys            map[string]bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.wantKey {
			if k, ok := tok.(string); ok {
				k = foldKey(k)
				if top.keys[k] {
					return errors.New("push: a key appears twice")
				}
				top.keys[k] = true
				top.wantKey = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			if top != nil && top.object {
				top.wantKey = true
			}
			stack = append(stack, &frame{object: true, wantKey: true, keys: map[string]bool{}})
			continue
		case json.Delim('['):
			if top != nil && top.object {
				top.wantKey = true
			}
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			continue
		}
		if top != nil && top.object {
			top.wantKey = true // a value ends: the next token is a key
		}
	}
}

// foldKey maps each rune of k to the smallest rune of its simple case
// folding orbit, so two keys encoding/json would match to one field fold to
// one string.
func foldKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// endpointHost is the only part of an endpoint an audit line may carry.
func endpointHost(endpoint string) string {
	if u, err := webpush.CheckEndpoint(endpoint, nil); err == nil {
		return u.Host
	}
	if i := strings.Index(endpoint, "://"); i >= 0 {
		h, _, _ := strings.Cut(endpoint[i+3:], "/")
		return truncateAudit(h)
	}
	return "?"
}
