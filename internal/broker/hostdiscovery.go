package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/mcp"
)

// A service host is often away (a laptop that sleeps). If it is down when an
// agent's MCP client starts or reconnects, the client's initialize and
// tools/list fail and it marks the whole server failed, so the host's tools
// stay missing after the host comes back. The gateway therefore keeps the
// last good initialize and tools/list answer of each service-host tool and,
// while the host cannot answer, serves those instead. Only discovery is
// served from the copy: tools/call still gets the 503, and works again on the
// first call after the host is back.
//
// No copy is kept for a tool whose server opens a session (its initialize
// answer carries an Mcp-Session-Id): a copied initialize has no session, so
// the client's calls after the host is back would be refused for the missing
// session, which is worse than the plain failure. Such a tool gets the 503
// as before.
//
// The copy is the service host's own answer, before the gateway adds the
// capability argument (that is added again when it is served), and it is
// kept in the broker's state directory so a broker restart while the host is
// away still has it.

// maxInitializeBytes bounds an initialize answer the gateway keeps.
const maxInitializeBytes = 1 << 20

// discoveryMethods are the methods answered from the copy.
var discoveryMethods = map[string]bool{"initialize": true, "tools/list": true}

// hostDiscovery is one service-host tool's last good discovery answers.
type hostDiscovery struct {
	path string // the file that keeps it; "" keeps it in memory only

	mu       sync.Mutex
	results  map[string]json.RawMessage // method → the answer's "result"
	sessions bool                       // the server opened a session: keep nothing
}

// hostDiscoveryFile is the on-disk form. Sessions marks a server seen to
// open sessions: nothing is kept for it.
type hostDiscoveryFile struct {
	Saved    time.Time                  `json:"saved"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Sessions bool                       `json:"sessions,omitempty"`
}

// loadHostDiscovery reads the copy at path. A missing, unreadable or
// malformed file is an empty copy: the next good answer replaces it.
func loadHostDiscovery(path string) *hostDiscovery {
	d := &hostDiscovery{path: path, results: map[string]json.RawMessage{}}
	if path == "" {
		return d
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > maxToolsListBytes+maxInitializeBytes+4096 {
		return d
	}
	var f hostDiscoveryFile
	if json.Unmarshal(raw, &f) != nil {
		return d
	}
	if f.Sessions {
		d.sessions = true
		return d
	}
	for m, r := range f.Results {
		if discoveryMethods[m] && validResult(r) {
			d.results[m] = r
		}
	}
	return d
}

func validResult(r json.RawMessage) bool {
	var v map[string]any
	return json.Unmarshal(r, &v) == nil && v != nil
}

// record keeps a good answer to the discovery request dr: a JSON-RPC
// response with an object "result". Anything else (an error answer, a batch,
// junk) is ignored, as is a page of a paginated tools/list and an initialize
// whose protocolVersion is not the one asked for (the copy is served to every
// agent, so one agent's choice of version must not become the others'). An
// answer equal to the kept one is not written again.
func (d *hostDiscovery) record(dr discoveryRequest, body []byte) error {
	method := dr.method
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.Error) != 0 || !validResult(msg.Result) {
		return nil
	}
	switch method {
	case "tools/list":
		if dr.cursor || resultField(msg.Result, "nextCursor") != nil {
			return nil
		}
	case "initialize":
		if v, _ := resultField(msg.Result, "protocolVersion").(string); v == "" || v != dr.version {
			return nil
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, msg.Result) != nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions {
		return nil
	}
	if bytes.Equal(d.results[method], compact.Bytes()) {
		return nil
	}
	d.results[method] = compact.Bytes()
	return d.saveLocked()
}

func (d *hostDiscovery) saveLocked() error {
	if d.path == "" {
		return nil
	}
	f := hostDiscoveryFile{Saved: time.Now().UTC(), Sessions: d.sessions}
	if !d.sessions {
		f.Results = d.results
	}
	out, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(d.path, out, 0o600)
}

// resultField is one top-level field of a result object, or nil.
func resultField(r json.RawMessage, name string) any {
	var m map[string]any
	if json.Unmarshal(r, &m) != nil {
		return nil
	}
	return m[name]
}

// forget drops the copy for good, on disk too: the tool's server opens
// sessions. The file then records only that, so a restarted broker keeps
// nothing for it either.
func (d *hostDiscovery) forget() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions {
		return nil
	}
	d.sessions = true
	d.results = map[string]json.RawMessage{}
	return d.saveLocked()
}

// answer builds the response to the request dr from the kept copy, with the
// capability argument added to a tools/list. ok is false without a copy, for
// a request without an id, for a later page of tools/list, and for an
// initialize asking another protocolVersion than the copy's (a newer client
// then fails as it would without the copy).
func (d *hostDiscovery) answer(dr discoveryRequest) ([]byte, bool) {
	method, id := dr.method, dr.id
	d.mu.Lock()
	r, ok := d.results[method]
	d.mu.Unlock()
	if !ok || len(id) == 0 || string(id) == "null" {
		return nil, false
	}
	switch method {
	case "tools/list":
		if dr.cursor {
			return nil, false
		}
	case "initialize":
		if v, _ := resultField(r, "protocolVersion").(string); v != dr.version {
			return nil, false
		}
	}
	if method == "tools/list" {
		// The capability argument is added to the result alone, so the
		// request's id is echoed byte for byte.
		aug := mcp.AugmentToolsListSchema(append(append([]byte(`{"jsonrpc":"2.0","id":null,"result":`), r...), '}'))
		var msg struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(aug, &msg) != nil || len(msg.Result) == 0 {
			return nil, false
		}
		r = msg.Result
	}
	out, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{"2.0", id, r})
	if err != nil {
		return nil, false
	}
	return out, true
}

// has reports whether any discovery answer is kept.
func (d *hostDiscovery) has() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.results) > 0
}

// requestID is the raw JSON-RPC id of body, so an answer echoes it exactly
// (a float64 round trip would change a large integer id).
func requestID(body []byte) json.RawMessage {
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return nil
	}
	return msg.ID
}

// discoveryRequest is what the gateway notes on a discovery request for the
// service host's answer and for the fallback.
type discoveryRequest struct {
	method  string
	id      json.RawMessage
	cursor  bool   // tools/list: params.cursor is set (a later page)
	version string // initialize: params.protocolVersion
}

// newDiscoveryRequest reads what a discovery request asks for.
func newDiscoveryRequest(method string, body []byte) discoveryRequest {
	dr := discoveryRequest{method: method, id: requestID(body)}
	var msg struct {
		Params struct {
			Cursor          json.RawMessage `json:"cursor"`
			ProtocolVersion string          `json:"protocolVersion"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) == nil {
		dr.cursor = len(msg.Params.Cursor) != 0 && string(msg.Params.Cursor) != "null"
		dr.version = msg.Params.ProtocolVersion
	}
	return dr
}

type discoveryKey struct{}

// captureDiscovery records a good answer to a discovery request and restores
// the body for the next step. It runs before the capability argument is
// added.
//
// Only an application/json answer is kept: an upstream that answers POST
// with an event stream never gets a copy. An answer over the bound passes
// through unrecorded.
func (d *hostDiscovery) captureDiscovery(resp *http.Response, onErr func(error)) error {
	if resp.Request == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	dr, ok := resp.Request.Context().Value(discoveryKey{}).(discoveryRequest)
	if !ok || !discoveryMethods[dr.method] {
		return nil
	}
	if dr.method == "initialize" && resp.Header.Get("Mcp-Session-Id") != "" {
		if err := d.forget(); err != nil && onErr != nil {
			onErr(err)
		}
		return nil
	}
	if mediaType(resp.Header.Get("Content-Type")) != "application/json" {
		return nil
	}
	limit := int64(maxInitializeBytes)
	if dr.method == "tools/list" {
		limit = maxToolsListBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		resp.Body.Close()
		return err
	}
	if int64(len(body)) > limit {
		// Too big to keep: pass it on whole, as before.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	if err := d.record(dr, body); err != nil && onErr != nil {
		onErr(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return nil
}

func mediaType(ct string) string {
	if i := bytes.IndexByte([]byte(ct), ';'); i >= 0 {
		ct = ct[:i]
	}
	return string(bytes.ToLower(bytes.TrimSpace([]byte(ct))))
}

// discoveryFromCopy answers the discovery request in ctx from d's copy. ok is
// false for any other request, or without a copy.
func (b *Broker) discoveryFromCopy(d *hostDiscovery, toolName string, ctx context.Context) ([]byte, bool) {
	if d == nil {
		return nil, false
	}
	dr, ok := ctx.Value(discoveryKey{}).(discoveryRequest)
	if !ok || !discoveryMethods[dr.method] {
		return nil, false
	}
	out, ok := d.answer(dr)
	if ok {
		b.log.Info("gateway.discovery_from_copy", "op", toolName, "method", dr.method, "caller", callerFrom(ctx))
	}
	return out, ok
}

func isUnavailable(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// replaceResponse turns resp into a 200 JSON answer with body out, or a
// bodiless 202 when out is nil.
func replaceResponse(resp *http.Response, out []byte) {
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}
	resp.Header = http.Header{}
	if out == nil {
		resp.StatusCode, resp.Status = http.StatusAccepted, "202 Accepted"
		resp.Body, resp.ContentLength = http.NoBody, 0
		resp.Header.Set("Content-Length", "0")
		return
	}
	resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
}

// initializedFromCopy reports whether the request in ctx is the
// notifications/initialized of a client that may have initialized from the
// copy: with the host unable to take it, it is answered 202 here, as the
// host would (a client fails its connect on any other answer).
func initializedFromCopy(d *hostDiscovery, ctx context.Context) bool {
	dr, ok := ctx.Value(discoveryKey{}).(discoveryRequest)
	return ok && dr.method == "notifications/initialized" && d != nil && d.has()
}
