package remoteproxy

import (
	"net/http"
	"strings"
)

// DecisionDenyPath is the audit decision for a request whose path the proxy
// refuses to read (see unsafePath).
const DecisionDenyPath Decision = "deny-path"

// unsafePath reports why the request's path is refused, or "" when it is not.
//
// Every route decision in the proxy (mintsCredential, refusedRoute, the chat
// page, the contact fence's allow-list) reads r.URL.Path as given, by prefix
// or by segment. A path that a later reader could resolve to a different
// route would pass under one name and land on another (for example
// /assets/../lever/api/chat under the contact's allowed "/assets/" prefix).
// The proxy does not rely on the hub's mux redirecting such paths: for every
// tier it refuses any path not already in the one spelling every reader
// agrees on:
//
//   - not rooted ("*", an opaque form);
//   - a "." or ".." segment, or an empty one ("//");
//   - a backslash or a NUL, decoded or not;
//   - an encoded slash, backslash, dot or NUL (%2f, %5c, %2e, %00) in the
//     raw path, which a reader that decodes before it splits would see as a
//     separator or a dot segment.
//
// What passes decodes to the same segments however it is read, so the
// decisions and the forwarded request (ReverseProxy sends r.URL's escaped
// path) name the same route. ServeHTTP runs it first, before authorize and
// every tier decision.
func unsafePath(r *http.Request) string {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/") {
		return "path is not rooted"
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "path holds a backslash or a NUL"
	}
	for _, seg := range strings.Split(p[1:], "/") {
		switch seg {
		case ".", "..":
			return "path holds a dot segment"
		}
	}
	if strings.Contains(p, "//") {
		return "path holds an empty segment"
	}
	raw := strings.ToLower(r.URL.EscapedPath())
	for _, enc := range []string{"%2f", "%5c", "%2e", "%00"} {
		if strings.Contains(raw, enc) {
			return "path holds an encoded slash, backslash, dot or NUL"
		}
	}
	return ""
}
