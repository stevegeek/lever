package remoteproxy

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The chat page.
//
// With Config.ChatAgent set (remote.landing: chat), the proxy serves a small
// page of its own under /lever/ and sends "/" there: one conversation, the
// operator's DM with the manager, plus links to the manager's terminal and to
// the hub's full web UI. It is an alternative front for a route that already
// exists, not a new route to the agent:
//
//   - The page reads and sends through the hub's own chat routes, forwarded
//     by this proxy like any other request. A message it posts is therefore
//     recorded by recordChat exactly as one posted from the hub's web UI, and
//     verifies the same way (package chatledger). Nothing here signs,
//     records or delivers a message.
//   - Everything below is answered only for an operator-tier login that
//     passed the gate. A contact never gets here: ServeHTTP hands its
//     requests to the contact fence instead, which answers every page with
//     its own landing page.
//   - What lever serves is fixed: the files embedded in the binary, and one
//     JSON answer (the bootstrap) built from the verified login and from two
//     ids the hub names, each checked before it is used. No agent-written
//     byte is served from here. The page writes what it reads from the hub as
//     text only (see chatui/chat.js), and its CSP allows no inline script.
//
// The whole /lever/ prefix is lever's while the page is on: an unknown path
// under it is a 404, never forwarded, so a route added here later cannot
// shadow one the hub answers.

// The page is installable as an app (a web app manifest and icons, below),
// with no service worker on purpose: a worker scoped to /lever/ would sit
// between the page and every request it makes, and a cache it keeps can
// serve a page older than the binary. Current browsers install without one.

//go:generate go run chaticons_gen.go
//go:embed chatui/chat.html chatui/chat.css chatui/chat.js chatui/chatcore.js chatui/*.png
var chatUI embed.FS

// DecisionChatUnavailable is the audit decision for a chat page request the
// proxy could not answer (the hub named no usable identity or agent).
const DecisionChatUnavailable Decision = "chat-unavailable"

const (
	chatPrefix        = "/lever/"
	chatPagePath      = "/lever/chat"
	chatBootstrapPath = "/lever/api/chat"
	chatManifestPath  = "/lever/manifest.webmanifest"
	// chatConsolePath is where the page's link to the hub's web UI goes: its
	// agent list, since "/" now leads back to the chat page.
	chatConsolePath = "/agents"
)

// chatCSPFor is the page's policy: it loads script and style only from
// lever's own /lever/ path, talks only to its own origin, and allows no
// inline script or style, no frame, no form target and no other origin.
//
// The path matters. The hub serves agent-written files under /api/ with a
// type taken from the file name, so on this origin 'self' would also admit a
// .js file an agent wrote. The page has no markup sink for an agent to name
// such a file through (TestChatPageHasNoMarkupSink); this keeps a sink added
// by mistake from loading agent script as well. The app manifest and its
// icons come from /lever/ too (manifest-src, img-src). A serveHost the
// policy cannot name (cspHost) falls back to 'self'.
//
// The policy names base_url's host, so the page runs only when opened there.
// Opened under another name the Host check admits (the loopback probe
// address, or a bind address), the browser blocks the page's own files:
// those names are for probes and fronts, not for a browser.
func chatCSPFor(serveHost string) string {
	own, img := "'self'", "'self'"
	if cspHost(serveHost) {
		own, img = serveHost+chatPrefix, serveHost+"/favicon.svg "+serveHost+chatPrefix
	}
	// Trusted Types with no policy allowed makes every markup or script sink
	// throw where the browser supports it: a second guard on the same rule.
	return "default-src 'none'; script-src " + own + "; style-src " + own + "; connect-src 'self'; " +
		"img-src " + img + "; manifest-src " + own + "; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; " +
		"require-trusted-types-for 'script'; trusted-types 'none'"
}

// cspHost reports whether host (a name or an IPv4 address, with an optional
// port) can be written into a CSP source as it is. An IPv6 literal cannot:
// the CSP host-source grammar has no brackets, and a browser drops a source
// it cannot parse, which would leave the page with no script at all.
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

// chatFile is one embedded file, read once.
type chatFile struct {
	contentType string
	body        []byte
	etag        string
}

// chatPage serves the page for one manager agent.
type chatPage struct {
	agent   string // the manager's agent name (Config.ChatAgent)
	csp     string
	files   map[string]chatFile
	resolve func(ctx context.Context) (map[string]string, error) // agent name → hub id
	whoAmI  func(ctx context.Context, cookie string) (string, error)
}

// newChatPage loads the embedded files. A file that is missing is a build
// fault, so it panics rather than serve a page with a hole in it.
func newChatPage(cfg Config) *chatPage {
	p := &chatPage{agent: cfg.ChatAgent, csp: chatCSPFor(cfg.ServeHost), resolve: cfg.ResolveAgents, whoAmI: hubWhoAmI(cfg), files: map[string]chatFile{}}
	add := func(route, contentType string, body []byte) {
		sum := sha256.Sum256(body)
		p.files[route] = chatFile{contentType: contentType, body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	}
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
	manifest, err := json.Marshal(chatManifestFor(cfg.ChatAgent))
	if err != nil {
		panic("remoteproxy: chat page manifest: " + err.Error())
	}
	add(chatManifestPath, "application/manifest+json", manifest)
	return p
}

// chatManifest is the page's web app manifest: what a browser needs to
// install the page as an app (Chrome's "Install page as app", Safari's "Add
// to Home Screen"). It opens /lever/chat in a window of its own; the
// Terminal and Console links leave its scope and still work, shown by the
// browser as pages outside the app.
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

// chatManifestFor names the app after the instance (the manager's agent
// name is the instance name, a short [a-z0-9-] token config validates). The
// colours are chat.css's light ones: --bg behind the window while it loads,
// --panel for the title bar, which the header continues. The dark scheme
// sets its own bar colour in chat.html; a manifest has one.
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

// chatBootstrap is what the page needs to find its conversation.
type chatBootstrap struct {
	Login  string       `json:"login"`
	UserID string       `json:"userId"`
	Agent  chatAgentRef `json:"agent"`
	// Conversation is the DM key, and Terminal the hub web UI's terminal
	// page for the agent. Both are empty while the agent has no hub record.
	Conversation string `json:"conversation,omitempty"`
	Terminal     string `json:"terminal,omitempty"`
	Console      string `json:"console"`
}

type chatAgentRef struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// validHubID reports whether an id the hub named is safe to put in a URL
// path and a conversation key: the page builds both from it, on the
// operator's origin. Hub ids are UUIDs; anything with a separator in it is
// refused rather than escaped.
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
// reports false for a request that goes to the hub as before. operator is
// the verified operator-tier login and cookie its hub session; the caller
// has already run authorize and ruled out a contact.
func (g *gate) serveChatPage(w http.ResponseWriter, r *http.Request, line *AuditLine, operator, cookie string) bool {
	if g.chat == nil || operator == "" {
		// No verified login (allowed_users empty): nothing the page sent
		// would verify, so there is no page.
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
	if !read {
		w.Header().Set("Allow", "GET, HEAD")
		g.answerChat(w, line, DecisionAllow, http.StatusMethodNotAllowed, nil, []byte("method not allowed\n"), r)
		return true
	}
	if p == chatBootstrapPath {
		g.serveChatBootstrap(w, r, line, operator, cookie)
		return true
	}
	f, ok := g.chat.files[p]
	if !ok {
		g.answerChat(w, line, DecisionAllow, http.StatusNotFound, nil, []byte("not found\n"), r)
		return true
	}
	hdr := func() {
		w.Header().Set("Content-Type", f.contentType)
		w.Header().Set("ETag", f.etag)
		// Revalidate on every load: the files change with the lever binary.
		// private: the answer at this URL depends on who asks (a contact
		// gets the fence's page), so no shared cache may keep it.
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	if etagMatches(r.Header.Get("If-None-Match"), f.etag) {
		g.answerChat(w, line, DecisionAllow, http.StatusNotModified, hdr, nil, r)
		return true
	}
	g.answerChat(w, line, DecisionAllow, http.StatusOK, hdr, f.body, r)
	return true
}

// etagMatches reports whether an If-None-Match value names etag: "*", or a
// list with the tag in it, strong or weak (a front that compresses the
// answer weakens the tag it passes on).
func etagMatches(header, etag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimSpace(v)
		if v == "*" || strings.TrimPrefix(v, "W/") == etag {
			return true
		}
	}
	return false
}

// answerChat writes one of the chat page's own answers and audits it. Every
// answer carries the page's CSP, whatever its type: a 404 text is then as
// inert as the page is strict. Nothing is stored unless headers says so.
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

// serveChatBootstrap answers the bootstrap: who the operator is to the hub,
// and which agent and conversation the page is for.
func (g *gate) serveChatBootstrap(w http.ResponseWriter, r *http.Request, line *AuditLine, operator, cookie string) {
	c := g.chat
	unavailable := func(msg string) {
		g.answerChat(w, line, DecisionChatUnavailable, http.StatusBadGateway, nil, []byte(msg+"\n"), r)
	}
	if c.resolve == nil {
		unavailable("the chat page cannot resolve its agent")
		return
	}
	uid, err := c.whoAmI(r.Context(), cookie)
	if errors.Is(err, errSessionUnknown) {
		// The hub no longer knows this session (it restarted, or the
		// session lapsed): replace it once, as forward does for a GET.
		g.cfg.Session.Invalidate(operator, cookie)
		if fresh, cerr := g.cfg.Session.Cookie(r.Context(), operator); cerr == nil {
			uid, err = c.whoAmI(r.Context(), fresh)
		}
	}
	if err != nil || !validHubID(uid) {
		unavailable("cannot resolve your hub user")
		return
	}
	ids, err := c.resolve(r.Context())
	if err != nil {
		unavailable("cannot resolve the manager agent")
		return
	}
	out := chatBootstrap{Login: operator, UserID: uid, Agent: chatAgentRef{Name: c.agent}, Console: chatConsolePath}
	// No id is not a fault: the manager has no hub record until its first
	// start. The page says so.
	if id := ids[c.agent]; id != "" {
		if !validHubID(id) {
			unavailable("the hub named an agent id the chat page cannot use")
			return
		}
		out.Agent.ID = id
		out.Conversation = "dm:agent:" + id + ":user:" + uid
		out.Terminal = "/agents/" + id + "/terminal"
	}
	body, err := json.Marshal(out)
	if err != nil {
		unavailable("cannot encode the chat page's data")
		return
	}
	g.answerChat(w, line, DecisionAllow, http.StatusOK, func() {
		w.Header().Set("Content-Type", "application/json")
		// Like every /api/ answer the proxy forwards (sandboxAPIDocument):
		// opened as a page, this is an inert document.
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, body, r)
}
