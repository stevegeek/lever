package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("reply is not JSON: %v: %s", err, b)
	}
	return m
}

func testService() Service {
	return Service{
		Name:  "svc",
		Tools: func() []any { return []any{map[string]any{"name": "t"}} },
		Call: func(_ context.Context, id any, msg map[string]any) []byte {
			name, _, _, _ := ToolsCall(msg)
			return TextResult(id, "called "+name)
		},
	}
}

func TestDispatchRoutesStandardMethods(t *testing.T) {
	ctx := context.Background()
	svc := testService()

	init := decode(t, Dispatch(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`), svc))
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersion || res["serverInfo"].(map[string]any)["name"] != "svc" {
		t.Fatalf("initialize result = %v", res)
	}
	if init["id"].(float64) != 1 {
		t.Fatalf("id not echoed: %v", init)
	}

	list := decode(t, Dispatch(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), svc))
	if tools := list["result"].(map[string]any)["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools/list = %v", list)
	}

	call := decode(t, Dispatch(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"t","arguments":{}}}`), svc))
	text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "called t" {
		t.Fatalf("tools/call text = %v", text)
	}

	unknown := decode(t, Dispatch(ctx, []byte(`{"jsonrpc":"2.0","id":4,"method":"nope"}`), svc))
	if code := unknown["error"].(map[string]any)["code"].(float64); int(code) != CodeMethodNotFound {
		t.Fatalf("unknown method code = %v", code)
	}

	bad := decode(t, Dispatch(ctx, []byte(`not json`), svc))
	e := bad["error"].(map[string]any)
	if int(e["code"].(float64)) != CodeParseError || e["message"] != "parse error" || bad["id"] != nil {
		t.Fatalf("parse error reply = %v", bad)
	}
}

func TestResultAndErrorFraming(t *testing.T) {
	r := decode(t, Result("abc", map[string]any{"x": 1}))
	if r["jsonrpc"] != "2.0" || r["id"] != "abc" || r["result"].(map[string]any)["x"].(float64) != 1 {
		t.Fatalf("Result = %v", r)
	}
	e := decode(t, Error(7, CodeServerError, "boom"))
	em := e["error"].(map[string]any)
	if e["id"].(float64) != 7 || int(em["code"].(float64)) != CodeServerError || em["message"] != "boom" {
		t.Fatalf("Error = %v", e)
	}
}

func TestServeHTTPBoundsBodyAndSetsContentType(t *testing.T) {
	handle := func(_ context.Context, body []byte) []byte { return Result(nil, string(body)) }

	w := httptest.NewRecorder()
	ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)), handle)
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got := decode(t, w.Body.Bytes())["result"]; got != "{}" {
		t.Fatalf("handle did not receive the body: %v", got)
	}

	w = httptest.NewRecorder()
	big := bytes.Repeat([]byte("a"), MaxBodyBytes+1)
	ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(big)), handle)
	e := decode(t, w.Body.Bytes())["error"].(map[string]any)
	if int(e["code"].(float64)) != CodeParseError || e["message"] != "read error" {
		t.Fatalf("oversized body reply = %v", e)
	}
}

func TestMapConstraintParamsIdentityAndRename(t *testing.T) {
	out := MapConstraintParams(map[string]string{"table": "schema.table"}, map[string]string{"schema.table": "A", "filter": "Y"})
	if out["table"] != "A" || out["schema.table"] != "A" || out["filter"] != "Y" || len(out) != 3 {
		t.Fatalf("MapConstraintParams = %v", out)
	}
	// A renamed arg that is absent produces no binding (fails closed at verify).
	out = MapConstraintParams(map[string]string{"table": "schema.table"}, map[string]string{"filter": "Y"})
	if _, ok := out["table"]; ok {
		t.Fatalf("absent renamed arg produced a binding: %v", out)
	}
}
