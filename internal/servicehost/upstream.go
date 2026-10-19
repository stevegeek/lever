package servicehost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// maxUpstreamBody bounds an upstream's answer, which the service host reads
// whole: it collapses an SSE stream to the one JSON-RPC response so it can
// filter tools/list and the broker sees plain JSON.
const maxUpstreamBody = 16 << 20

// upstreamServer forwards to a streamable-HTTP MCP server on (or reachable
// from) the service host, adding its configured credentials.
type upstreamServer struct {
	tool   Tool
	client *http.Client
}

func newUpstreamServer(t Tool) *upstreamServer {
	return &upstreamServer{tool: t, client: &http.Client{
		// A redirect would take the credential headers somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                 nil,
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}}
}

// headers reads the credential files on every call, so a rotated secret
// needs no restart.
func (u *upstreamServer) headers() (http.Header, error) {
	h := http.Header{}
	for k, v := range u.tool.Headers {
		h.Set(k, v)
	}
	for k, p := range u.tool.HeaderFiles {
		v, err := readSecretValue(p, false)
		if err != nil {
			return nil, fmt.Errorf("header_files %s: %w", k, err)
		}
		h.Set(k, v)
	}
	return h, nil
}

type upstreamReply struct {
	status    int
	sessionID string
	body      []byte // a JSON-RPC message, or empty
	ctype     string
}

var errUpstreamDown = errors.New("upstream unavailable")

// forward sends body and returns the upstream's answer. Only the session and
// protocol-version headers of the inbound request pass: anything else the
// agent or the broker sent stays here.
func (u *upstreamServer) forward(ctx context.Context, in *http.Request, body []byte, id any) (upstreamReply, error) {
	h, err := u.headers()
	if err != nil {
		return upstreamReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.tool.Upstream, bytes.NewReader(body))
	if err != nil {
		return upstreamReply{}, err
	}
	req.Header = h
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, k := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if v := in.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := u.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return upstreamReply{}, errTimeout
		}
		return upstreamReply{}, fmt.Errorf("%w: %v", errUpstreamDown, err)
	}
	defer resp.Body.Close()
	rep := upstreamReply{status: resp.StatusCode, sessionID: resp.Header.Get("Mcp-Session-Id")}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return upstreamReply{}, errTimeout
		}
		return upstreamReply{}, fmt.Errorf("%w: %v", errUpstreamDown, err)
	}
	if len(raw) > maxUpstreamBody {
		return upstreamReply{}, errors.New("upstream answer too large")
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case mt == "text/event-stream":
		msg, ok := responseFromSSE(raw, id)
		if !ok {
			return upstreamReply{}, errors.New("upstream event stream held no response")
		}
		rep.body, rep.ctype = msg, "application/json"
	case mt == "application/json":
		rep.body, rep.ctype = raw, "application/json"
	default:
		// An error page or an empty 202: its status is what matters.
		rep.body, rep.ctype = nil, ""
		if resp.StatusCode >= 400 && len(raw) > 0 {
			rep.body, rep.ctype = clip(raw, 512), "text/plain; charset=utf-8"
		}
	}
	return rep, nil
}

func clip(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// responseFromSSE finds the JSON-RPC response to id in an event stream.
// Notifications and server requests in the stream are dropped.
func responseFromSSE(raw []byte, id any) ([]byte, bool) {
	want, _ := json.Marshal(id)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), maxUpstreamBody)
	var data []string
	flush := func() ([]byte, bool) {
		if len(data) == 0 {
			return nil, false
		}
		ev := strings.Join(data, "\n")
		data = data[:0]
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(ev), &m) != nil {
			return nil, false
		}
		_, isReq := m["method"]
		gotID, hasID := m["id"]
		if isReq || !hasID || !jsonEqual(gotID, want) {
			return nil, false
		}
		return []byte(ev), true
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if msg, ok := flush(); ok {
				return msg, true
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	return flush()
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
