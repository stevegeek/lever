package servicehost

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// InfoPath answers the paired broker's `lever doctor` with the tools served
// and their ceilings. The broker's gateway never routes an agent here: it
// pins every tool call to /mcp/<tool>.
const InfoPath = "/lever/v1/info"

// ToolPath is where tool name is served.
func ToolPath(name string) string { return "/mcp/" + name }

// maxRequestBody matches the broker gateway's bound on one message.
const maxRequestBody = 1 << 20

// maxInFlight bounds the calls one tool holds open at once, so a flood
// from the paired broker cannot pile up without limit in front of one
// server.
const maxInFlight = 64

// Info is the InfoPath answer.
type Info struct {
	Tools []InfoTool `json:"tools"`
}

type InfoTool struct {
	Name  string   `json:"name"`
	Allow []string `json:"allow"`
}

// CallerHeader carries the calling agent's name from the broker. The service
// host only logs it: the ceiling is per instance, not per agent.
const CallerHeader = "X-Lever-Caller"

var callerRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Server serves one service host config.
type Server struct {
	cfg      *Config
	pin      Fingerprint
	log      *slog.Logger
	stdio    map[string]*stdioServer
	upstream map[string]*upstreamServer
	inFlight map[string]chan struct{}
}

// NewServer builds the handler state for cfg. version is reported to stdio
// servers as the client version.
func NewServer(cfg *Config, log *slog.Logger, version string) (*Server, error) {
	pin, err := ParseFingerprint(cfg.ClientFingerprint)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, pin: pin, log: log, stdio: map[string]*stdioServer{}, upstream: map[string]*upstreamServer{}, inFlight: map[string]chan struct{}{}}
	for _, t := range cfg.Tools {
		s.inFlight[t.Name] = make(chan struct{}, maxInFlight)
		if len(t.Command) > 0 {
			s.stdio[t.Name] = &stdioServer{tool: t, defDir: cfg.dir, log: log, version: version}
		} else {
			s.upstream[t.Name] = newUpstreamServer(t)
		}
	}
	return s, nil
}

// Close stops every stdio server.
func (s *Server) Close() {
	for _, st := range s.stdio {
		st.close()
	}
}

func (s *Server) audit(decision, tool, op, caller, detail string) {
	attrs := []any{"decision", decision, "tool", tool}
	if op != "" {
		attrs = append(attrs, "op", truncate(op, 128))
	}
	if caller != "" {
		attrs = append(attrs, "caller", caller)
	}
	if detail != "" {
		attrs = append(attrs, "detail", detail)
	}
	s.log.Info("servicehost.audit", attrs...)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ServeHTTP is the whole surface: the tool routes and InfoPath.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The handshake already checked the pin; this keeps a handler mounted
	// on some other listener from serving anyone.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || !s.pin.matches(r.TLS.PeerCertificates[0]) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == InfoPath {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveInfo(w)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	t, known := s.cfg.tool(name)
	if !ok || !known || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	caller := r.Header.Get(CallerHeader)
	if !callerRE.MatchString(caller) {
		caller = ""
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, ok := decodeMessage(body)
	if !ok {
		s.audit("deny", t.Name, "", caller, "not a JSON-RPC message")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	method, _ := msg["method"].(string)
	id, hasID := msg["id"]
	switch method {
	case "tools/call":
		op := toolsCallName(msg)
		if op == "" || !hasID {
			s.audit("deny", t.Name, "", caller, "malformed tools/call")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !t.allows(op) {
			s.audit("deny", t.Name, op, caller, "outside the ceiling")
			http.Error(w, "refused by the service host's ceiling", http.StatusForbidden)
			return
		}
		s.audit("allow", t.Name, op, caller, "")
	case "tools/list", "initialize", "ping":
		if !hasID {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	case "":
		// A response from the client: the service host never asks one.
		w.WriteHeader(http.StatusAccepted)
		return
	default:
		if !hasID {
			// notifications/initialized and the rest: nothing to forward.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.log.Debug("servicehost.unsupported", "tool", t.Name, "method", truncate(method, 64), "caller", caller)
		writeJSON(w, http.StatusOK, rpcError(id, -32601, "method not found"))
		return
	}
	// The forwarded message is the one checked, re-encoded, so a server
	// that parses duplicate keys differently cannot be shown another call.
	canon, err := json.Marshal(msg)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	select {
	case s.inFlight[t.Name] <- struct{}{}:
		defer func() { <-s.inFlight[t.Name] }()
	default:
		s.log.Warn("servicehost.busy", "tool", t.Name, "in_flight", maxInFlight)
		http.Error(w, "tool "+t.Name+": busy; try again later", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.callTimeout())
	defer cancel()
	if st, ok := s.stdio[t.Name]; ok {
		s.serveStdio(ctx, w, st, t, method, msg, id)
		return
	}
	s.serveUpstream(ctx, w, r, s.upstream[t.Name], t, method, canon, id)
}

func (s *Server) serveStdio(ctx context.Context, w http.ResponseWriter, st *stdioServer, t Tool, method string, msg map[string]any, id any) {
	pv := "2025-06-18"
	if method == "initialize" {
		if params, ok := msg["params"].(map[string]any); ok {
			if v, ok := params["protocolVersion"].(string); ok && len(v) <= 32 && v != "" {
				pv = v
			}
		}
	}
	if method == "ping" {
		writeJSON(w, http.StatusOK, rpcResult(id, json.RawMessage(`{}`)))
		return
	}
	p, err := st.running(ctx, pv)
	if err != nil {
		s.fail(w, t, err)
		return
	}
	if method == "initialize" {
		writeJSON(w, http.StatusOK, rpcResult(id, p.initResult))
		return
	}
	resp, err := p.roundTrip(ctx, msg)
	if err != nil {
		s.fail(w, t, err)
		return
	}
	if method == "tools/list" {
		resp = filterToolsList(resp, t)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) serveUpstream(ctx context.Context, w http.ResponseWriter, r *http.Request, u *upstreamServer, t Tool, method string, body []byte, id any) {
	rep, err := u.forward(ctx, r, body, id)
	if err != nil {
		s.fail(w, t, err)
		return
	}
	if rep.sessionID != "" {
		w.Header().Set("Mcp-Session-Id", rep.sessionID)
	}
	if method == "tools/list" && rep.status == http.StatusOK && rep.ctype == "application/json" {
		rep.body = filterToolsList(rep.body, t)
	}
	if rep.ctype != "" {
		w.Header().Set("Content-Type", rep.ctype)
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.body)
}

// fail answers a call the tool could not serve. The cause goes to the log
// only: it can name paths and programs on this machine.
func (s *Server) fail(w http.ResponseWriter, t Tool, err error) {
	s.log.Warn("servicehost.call_failed", "tool", t.Name, "err", err.Error())
	if errors.Is(err, errTimeout) {
		http.Error(w, "tool "+t.Name+": call timed out", http.StatusGatewayTimeout)
		return
	}
	http.Error(w, "tool "+t.Name+": backend unavailable; try again later", http.StatusServiceUnavailable)
}

func (s *Server) serveInfo(w http.ResponseWriter) {
	info := Info{Tools: make([]InfoTool, 0, len(s.cfg.Tools))}
	for _, t := range s.cfg.Tools {
		info.Tools = append(info.Tools, InfoTool{Name: t.Name, Allow: t.Allow})
	}
	b, _ := json.Marshal(info)
	writeJSON(w, http.StatusOK, b)
}

// decodeMessage keeps numbers as written, so re-encoding the checked
// message changes no argument value.
func decodeMessage(body []byte) (map[string]any, bool) {
	if FoldedKeyCollision(body) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil || msg == nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return knownFields(msg), true
}

// knownFields keeps only the message fields MCP defines, so a key the
// ceiling check never read cannot reach the server.
func knownFields(msg map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"jsonrpc", "id", "method", "params"} {
		if v, ok := msg[k]; ok {
			out[k] = v
		}
	}
	if out["method"] == "tools/call" {
		if params, ok := out["params"].(map[string]any); ok {
			kept := map[string]any{}
			for _, k := range []string{"name", "arguments", "_meta"} {
				if v, ok := params[k]; ok {
					kept[k] = v
				}
			}
			out["params"] = kept
		}
	}
	return out
}

func toolsCallName(msg map[string]any) string {
	params, _ := msg["params"].(map[string]any)
	name, _ := params["name"].(string)
	return name
}

// filterToolsList drops the tools outside the ceiling from a tools/list
// response. A response it cannot read is replaced by an error rather than
// passed on unfiltered.
func filterToolsList(resp []byte, t Tool) []byte {
	if len(t.Allow) == 1 && t.Allow[0] == AllOperations {
		return resp
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resp, &m); err != nil {
		return rpcError(nil, -32603, "unreadable tools/list response")
	}
	if _, isErr := m["error"]; isErr {
		return resp
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(m["result"], &result); err != nil {
		return rpcError(m["id"], -32603, "unreadable tools/list response")
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		return rpcError(m["id"], -32603, "unreadable tools/list response")
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &tool) == nil && t.allows(tool.Name) {
			kept = append(kept, raw)
		}
	}
	result["tools"], _ = json.Marshal(kept)
	m["result"], _ = json.Marshal(result)
	out, _ := json.Marshal(m)
	return out
}

func rpcResult(id any, result json.RawMessage) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

func rpcError(id any, code int, message string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	return b
}

func writeJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// Serve listens on cfg.Listen until ctx ends.
func Serve(ctx context.Context, cfg *Config, log *slog.Logger, version string) error {
	certPEM, err := readChecked(cfg.CertPath(), false)
	if err != nil {
		return err
	}
	keyPEM, err := readPrivate(cfg.KeyPath())
	if err != nil {
		return err
	}
	cert, _, err := LoadKeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("server key pair: %w", err)
	}
	srv, err := NewServer(cfg, log, version)
	if err != nil {
		return err
	}
	defer srv.Close()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.With("op", "servicehost.http").Handler(), slog.LevelWarn),
	}
	log.Info("servicehost.listening", "addr", ln.Addr().String(), "tools", len(cfg.Tools))
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(tls.NewListener(ln, ServerTLSConfig(cert, srv.pin))) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
		return nil
	}
}
