package remoteproxy

import (
	"net/http"
	"strings"
)

// DecisionDenyPath audits a path unsafePath refuses.
const DecisionDenyPath Decision = "deny-path"

// unsafePath reports why the request's path is refused, or "".
//
// Every route decision reads r.URL.Path by prefix or segment, so a path a
// later reader could resolve differently would pass under one route and land
// on another (/assets/../lever/api/chat). Refused for every tier, without
// relying on the hub's mux: unrooted paths; ".", ".." or empty segments; a
// backslash or NUL; and %2f, %5c, %2e or %00 in the raw path. What passes
// reads the same however decoded, so decisions and the forwarded request
// name the same route. ServeHTTP runs it before every other check.
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
