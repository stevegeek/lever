package remoteproxy

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// The chat page. With Config.ChatAgent set (remote.landing: chat), the proxy
// serves its own page under /lever/ and sends "/" there. It is an alternative
// front for existing routes, not a new route to an agent:
//
//   - It reads and sends through the hub's own chat routes, forwarded like
//     any request; a contact's go through the contact fence (contact.go).
//     Posted messages are recorded by recordChat as from the hub's web UI.
//     Nothing here signs, records or delivers a message.
//   - Everything is answered only for a verified login that passed the gate,
//     built server-side from its tier and lists (agents.go): a contact never
//     learns of an agent outside its lists.
//   - lever serves only fixed bytes: embedded files, a manifest per tier and
//     the agent list (config, hub records reduced to fixed words, cleaned
//     labels). The page writes hub data as text only, and its CSP allows no
//     inline script.
//
// The whole /lever/ prefix is lever's: an unknown path under it is a 404,
// never forwarded, so a route added here cannot shadow a hub route. The
// service worker exists only with remote.push and is push-only (no fetch
// handler, no cache), so it cannot serve a stale page.

//go:generate go run chaticons_gen.go
//go:embed chatui/chat.html chatui/chat.css chatui/chat.js chatui/chatcore.js chatui/sw.js chatui/*.png
var chatUI embed.FS

// DecisionChatUnavailable audits a chat page request the proxy could not
// answer (no usable identity or agent from the hub).
const DecisionChatUnavailable Decision = "chat-unavailable"

const (
	chatPrefix       = "/lever/"
	chatPagePath     = "/lever/chat"
	chatAgentsPath   = "/lever/api/agents"
	chatManifestPath = "/lever/manifest.webmanifest"
	// chatConsolePath is the hub's agent list, since "/" leads to the chat page.
	chatConsolePath = "/agents"
)

// chatCSPFor is the page's policy: script, style and manifest only from
// lever's own /lever/ path, connections only to its own origin, nothing
// inline, no frame, no form target. The path matters: the hub serves
// agent-written files under /api/ with a type from the file name, so 'self'
// would admit an agent's .js. A serveHost the policy cannot name (cspHost)
// falls back to 'self'. Opened under any name but base_url's host, the page's
// own files are blocked; those names are not for a browser.
//
// With push on, worker-src names /lever/ and Trusted Types allows exactly
// lever-sw, whose only output is the literal worker URL, because
// serviceWorker.register is a script-URL sink.
func chatCSPFor(serveHost string, push bool) string {
	own, img := "'self'", "'self'"
	if cspHost(serveHost) {
		own, img = serveHost+chatPrefix, serveHost+"/favicon.svg "+serveHost+chatPrefix
	}
	worker, tt := "", "'none'"
	if push {
		worker, tt = "worker-src "+own+"; ", chatSWPolicy
	}
	// Trusted Types with no policy allowed makes every markup or script sink
	// throw: a second guard on the same rule.
	return "default-src 'none'; script-src " + own + "; style-src " + own + "; connect-src 'self'; " +
		"img-src " + img + "; manifest-src " + own + "; " + worker + "base-uri 'none'; form-action 'none'; frame-ancestors 'none'; " +
		"require-trusted-types-for 'script'; trusted-types " + tt
}

// chatSWPolicy only turns the literal "/lever/sw.js" into a script URL
// (TestChatPageHasNoMarkupSink pins it).
const chatSWPolicy = "lever-sw"

// cspHost reports whether host can be written into a CSP source as is. An
// IPv6 literal cannot: the grammar has no brackets, and a browser drops a
// source it cannot parse, leaving the page with no script.
func cspHost(host string) bool {
	if host == "" || len(host) > 255 {
		return false
	}
	for _, c := range []byte(host) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == ':':
		default:
			return false
		}
	}
	// At most one colon, before a port: "name:8445", never "fd7a::1" or "https:".
	if name, port, found := strings.Cut(host, ":"); found {
		if name == "" || port == "" || strings.Contains(port, ":") {
			return false
		}
		for _, c := range []byte(port) {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

type chatFile struct {
	contentType string
	body        []byte
	etag        string
}

type chatPage struct {
	csp   string
	files map[string]chatFile
	// manifests is per tier: a contact's is generic so it names no agent.
	manifests map[string]chatFile
	whoAmI    func(ctx context.Context, cookie string) (string, error)
	// hubGet and hubBody read hub routes with a login's own session; hubBody
	// is GET only.
	hubGet  func(ctx context.Context, cookie, path string, out any) (int, error)
	hubBody func(ctx context.Context, cookie, path string) (int, []byte, error)

	mu      sync.Mutex
	users   map[string]contactUser  // login → hub user id, for the agent list
	answers map[string]cachedAnswer // login → its last agent list (agentsAnswerTTL)
	nowFn   func() time.Time
}

// newChatPage panics on a missing embedded file: that is a build fault.
func newChatPage(cfg Config) *chatPage {
	p := &chatPage{csp: chatCSPFor(cfg.ServeHost, cfg.Push != nil), whoAmI: hubWhoAmI(cfg),
		hubGet: hubGetJSON(cfg), hubBody: hubGetBody(cfg, maxHistoryAnswer), files: map[string]chatFile{}, manifests: map[string]chatFile{}}
	file := func(contentType string, body []byte) chatFile {
		sum := sha256.Sum256(body)
		return chatFile{contentType: contentType, body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	}
	add := func(route, contentType string, body []byte) { p.files[route] = file(contentType, body) }
	for route, f := range map[string]struct{ name, contentType string }{
		chatPagePath:                   {"chatui/chat.html", "text/html; charset=utf-8"},
		"/lever/chat.css":              {"chatui/chat.css", "text/css; charset=utf-8"},
		"/lever/chat.js":               {"chatui/chat.js", "text/javascript; charset=utf-8"},
		"/lever/chatcore.js":           {"chatui/chatcore.js", "text/javascript; charset=utf-8"},
		"/lever/icon-192.png":          {"chatui/icon-192.png", "image/png"},
		"/lever/icon-512.png":          {"chatui/icon-512.png", "image/png"},
		"/lever/icon-maskable-512.png": {"chatui/icon-maskable-512.png", "image/png"},
		"/lever/apple-touch-icon.png":  {"chatui/apple-touch-icon.png", "image/png"},
	} {
		body, err := chatUI.ReadFile(f.name)
		if err != nil {
			panic("remoteproxy: embedded chat page file: " + err.Error())
		}
		add(route, f.contentType, body)
	}
	if cfg.Push != nil {
		body, err := chatUI.ReadFile("chatui/sw.js")
		if err != nil {
			panic("remoteproxy: embedded service worker: " + err.Error())
		}
		add(chatSWPath, "text/javascript; charset=utf-8", body)
	}
	for tier, name := range map[string]string{chatledger.TierOperator: cfg.ChatAgent, chatledger.TierContact: contactAppName} {
		manifest, err := json.Marshal(chatManifestFor(name))
		if err != nil {
			panic("remoteproxy: chat page manifest: " + err.Error())
		}
		p.manifests[tier] = file("application/manifest+json", manifest)
	}
	return p
}

// contactAppName is generic: the manager may be outside a contact's lists.
const contactAppName = "Chat"

type chatManifest struct {
	Name            string             `json:"name"`
	ShortName       string             `json:"short_name"`
	ID              string             `json:"id"`
	StartURL        string             `json:"start_url"`
	Scope           string             `json:"scope"`
	Display         string             `json:"display"`
	BackgroundColor string             `json:"background_color"`
	ThemeColor      string             `json:"theme_color"`
	Icons           []chatManifestIcon `json:"icons"`
}

type chatManifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

// chatManifestFor names the app after the instance (a config-validated
// [a-z0-9-] token). Colours are chat.css's light ones; chat.html sets the
// dark bar colour, since a manifest has one.
func chatManifestFor(instance string) chatManifest {
	short := instance
	if r := []rune(short); len(r) > 12 {
		short = string(r[:12])
	}
	icon := func(src, sizes, purpose string) chatManifestIcon {
		return chatManifestIcon{Src: chatPrefix + src, Sizes: sizes, Type: "image/png", Purpose: purpose}
	}
	return chatManifest{
		Name: instance + " · lever", ShortName: short,
		ID: chatPagePath, StartURL: chatPagePath, Scope: chatPrefix, Display: "standalone",
		BackgroundColor: "#f6f6f4", ThemeColor: "#ffffff",
		Icons: []chatManifestIcon{
			icon("icon-192.png", "192x192", "any"),
			icon("icon-512.png", "512x512", "any"),
			icon("icon-maskable-512.png", "512x512", "maskable"),
		},
	}
}

// validHubID reports whether a hub id is safe to put in a URL path and a
// conversation key on the operator's origin. Hub ids are UUIDs; anything with
// a separator is refused rather than escaped.
func validHubID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// serveChatPage answers a request the chat page owns and reports true, or
// false for one that goes on (to the contact fence or the hub). The caller
// has already run authorize.
func (g *gate) serveChatPage(w http.ResponseWriter, r *http.Request, line *AuditLine, login, cookie string) bool {
	if g.chat == nil || login == "" {
		// No verified login (allowed_users empty): nothing the page sent would
		// verify.
		return false
	}
	p := r.URL.Path
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if p == "/" {
		if !read {
			return false
		}
		// A fixed target: nothing of the request goes into Location.
		g.answerChat(w, line, DecisionAllow, http.StatusFound, func() { w.Header().Set("Location", chatPagePath) }, nil, r)
		return true
	}
	if p != strings.TrimSuffix(chatPrefix, "/") && !strings.HasPrefix(p, chatPrefix) {
		return false
	}
	v := g.viewerFor(login)
	if p == chatContactsPath || strings.HasPrefix(p, chatContactsPath+"/") {
		// Before the method check: a contact gets the fence's refusal whatever
		// it sends.
		g.serveOperatorView(w, r, line, v)
		return true
	}
	if name, under := wakeTarget(p); under {
		if name == "" {
			g.answerChat(w, line, DecisionAllow, http.StatusNotFound, nil, []byte("not found\n"), r)
			return true
		}
		g.serveWake(w, r, line, v, name)
		return true
	}
	if agent, id, under := filesTarget(p); under {
		if !g.filesOnFor(v.login) || agent == "" {
			// The prefix is lever's even when off; never forward it.
			g.answerChat(w, line, DecisionAllow, http.StatusNotFound, nil, []byte("not found\n"), r)
			return true
		}
		g.serveFiles(w, r, line, v, agent, id)
		return true
	}
	if p == strings.TrimSuffix(voicePrefix, "/") || strings.HasPrefix(p, voicePrefix) {
		switch {
		case g.voice != nil && p == voicePath:
			g.serveVoice(w, r, line, v)
		case g.speak != nil && p == speakPath:
			g.serveSpeak(w, r, line, v)
		default:
			// The prefix is lever's even when off; never forward it.
			g.answerChat(w, line, DecisionAllow, http.StatusNotFound, nil, []byte("not found\n"), r)
		}
		return true
	}
	if g.push != nil && (p == pushKeyPath || p == pushSubsPath) {
		g.servePush(w, r, line, v, p)
		return true
	}
	if !read {
		w.Header().Set("Allow", "GET, HEAD")
		g.answerChat(w, line, DecisionAllow, http.StatusMethodNotAllowed, nil, []byte("method not allowed\n"), r)
		return true
	}
	if p == chatAgentsPath {
		g.serveAgents(w, r, line, v, cookie)
		return true
	}
	f, ok := g.chat.files[p]
	if p == chatManifestPath {
		f, ok = g.chat.manifests[v.tier]
	}
	if !ok {
		g.answerChat(w, line, DecisionAllow, http.StatusNotFound, nil, []byte("not found\n"), r)
		return true
	}
	hdr := func() {
		w.Header().Set("Content-Type", f.contentType)
		w.Header().Set("ETag", f.etag)
		// private: the manifest differs per tier. No Vary: the login comes
		// from the front's identity header, and each tier's manifest has
		// its own ETag, so a changed login never gets a stale 304.
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	if etagMatches(r.Header.Get("If-None-Match"), f.etag) {
		g.answerChat(w, line, DecisionAllow, http.StatusNotModified, hdr, nil, r)
		return true
	}
	g.answerChat(w, line, DecisionAllow, http.StatusOK, hdr, f.body, r)
	return true
}

// etagMatches accepts weak tags: a compressing front weakens the tag.
func etagMatches(header, etag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimSpace(v)
		if v == "*" || strings.TrimPrefix(v, "W/") == etag {
			return true
		}
	}
	return false
}

// answerChat audits and writes one of the page's own answers. Every answer
// carries the page's CSP, so even a 404 text is inert.
func (g *gate) answerChat(w http.ResponseWriter, line *AuditLine, decision Decision, status int, headers func(), body []byte, r *http.Request) {
	line.Decision, line.Status = decision, status
	g.audit(*line)
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Security-Policy", g.chat.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	if headers != nil {
		headers()
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
