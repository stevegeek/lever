package remoteproxy

import (
	"net/http"
	"strings"
)

const chatAgentsPrefix = chatAgentsPath + "/"

// wakeTarget reports whether p is /lever/api/agents/<name>/wake. The wake
// route itself comes with the next change; until then every such path is
// a 404 from serveWake.
func wakeTarget(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, chatAgentsPrefix)
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, "/wake")
	return name, ok
}

func (g *gate) serveWake(w http.ResponseWriter, r *http.Request, line *AuditLine, _ viewer, _ string) {
	g.answerChat(w, line, DecisionAllow, http.StatusNotFound, nil, []byte("not found\n"), r)
}
