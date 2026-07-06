package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/scion"
)

// msgBroker's fixture deliberately makes the manager cert CN ("manager") and
// the manager scion agent SLUG ("assistant", the app name) DIFFER: a live bug
// (scion: `Agent "manager" not found in project`) hid behind an earlier
// fixture where CN == slug, so routing to agent:<CN> passed by coincidence.
func msgBroker(g2g bool) *Broker {
	b := New(Config{ManagerIdentity: "manager", ManagerSlug: "assistant", GroveToGrove: g2g, ManagerProject: "/lever",
		Groves: []GroveSpec{{Name: "scratch", JailProject: "/lever/groves/scratch"},
			{Name: "worker", JailProject: "/lever/groves/worker"}}})
	return b
}

func TestResolveMsgTarget(t *testing.T) {
	cases := []struct {
		name, caller, to string
		g2g              bool
		wantTo, wantProj string
		wantErr          bool
	}{
		{"manager to grove bare", "manager", "scratch", true, "agent:scratch", "/lever/groves/scratch", false},
		{"manager to grove prefixed", "manager", "agent:scratch", true, "agent:scratch", "/lever/groves/scratch", false},
		{"manager to manager by slug", "manager", "assistant", true, "agent:assistant", "/lever", false},
		{"manager to manager slug prefixed", "manager", "agent:assistant", true, "agent:assistant", "/lever", false},
		{"manager to manager by CN", "manager", "manager", true, "agent:assistant", "/lever", false},
		{"manager to user alias+CN", "manager", "user:manager", true, "agent:assistant", "/lever", false},
		{"manager to user slug", "manager", "user:assistant", true, "agent:assistant", "/lever", false},
		{"manager to user other", "manager", "user:stephen", true, "", "", true},
		{"manager to unknown grove", "manager", "nope", true, "", "", true},
		{"grove to manager by slug", "scratch", "agent:assistant", true, "agent:assistant", "/lever", false},
		{"grove to manager by CN", "scratch", "agent:manager", true, "agent:assistant", "/lever", false},
		{"grove to user", "scratch", "user:manager", true, "agent:assistant", "/lever", false},
		{"grove to grove allowed", "scratch", "worker", true, "agent:worker", "/lever/groves/worker", false},
		{"grove to grove disabled", "scratch", "worker", false, "", "", true},
		{"grove to itself", "scratch", "scratch", true, "agent:scratch", "/lever/groves/scratch", false},
		{"unknown caller", "mallory", "assistant", true, "", "", true},
		{"caller by slug is not an identity", "assistant", "scratch", true, "", "", true},
		{"grove to unknown", "scratch", "nope", true, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := msgBroker(c.g2g).resolveMsgTarget(c.caller, c.to)
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && (tgt.scionTo != c.wantTo || tgt.project != c.wantProj) {
				t.Fatalf("got (%q,%q), want (%q,%q)", tgt.scionTo, tgt.project, c.wantTo, c.wantProj)
			}
		})
	}
}

func TestResolveListProject(t *testing.T) {
	cases := []struct {
		name, caller, grove string
		want                string
		wantErr             bool
	}{
		{"manager own inbox", "manager", "", "/lever", false},
		{"manager reads grove", "manager", "scratch", "/lever/groves/scratch", false},
		{"manager unknown grove", "manager", "nope", "", true},
		{"grove own inbox", "scratch", "", "/lever/groves/scratch", false},
		{"grove may not target others", "scratch", "worker", "", true},
		{"unknown caller", "mallory", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := msgBroker(true).resolveListProject(c.caller, c.grove)
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("project = %q, want %q", got, c.want)
			}
		})
	}
}

// fakeMsgRuntime embeds the package's existing fakeRuntime (via the GroveRuntime
// interface field) for lifecycle methods it never exercises, and overrides
// Message/Inbox to capture what the msg handlers pass through.
type fakeMsgRuntime struct {
	GroveRuntime
	sent         []scion.MsgOpts
	events       []scion.Event
	inboxProject string
	sendErr      error
	inboxErr     error
}

func (f *fakeMsgRuntime) Message(_ context.Context, o scion.MsgOpts) error {
	f.sent = append(f.sent, o)
	return f.sendErr
}
func (f *fakeMsgRuntime) Inbox(_ context.Context, _ bool, project string) ([]scion.Event, error) {
	f.inboxProject = project
	return f.events, f.inboxErr
}

// newMsgTestBroker builds a Broker wired with a fakeMsgRuntime for the
// scratch/worker groves under manager cert CN "manager" and manager scion
// slug "assistant" (deliberately distinct, see msgBroker), capturing audit
// output to the returned buffer.
func newMsgTestBroker(g2g bool) (*Broker, *fakeMsgRuntime, *bytes.Buffer) {
	var buf bytes.Buffer
	rt := &fakeMsgRuntime{GroveRuntime: &fakeRuntime{agents: map[string][]scion.Agent{}}}
	b := New(Config{
		ManagerIdentity: "manager",
		ManagerSlug:     "assistant",
		ManagerProject:  "/lever",
		GroveToGrove:    g2g,
		Groves: []GroveSpec{
			{Name: "scratch", JailProject: "/lever/groves/scratch"},
			{Name: "worker", JailProject: "/lever/groves/worker"},
		},
		Runtime:  rt,
		Registry: registry.New(),
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	return b, rt, &buf
}

func TestMsgSend_managerToGrove(t *testing.T) {
	b, rt, _ := newMsgTestBroker(true)
	rec := callGrove(t, b, "/msg/send", `{"to":"scratch","body":"go","interrupt":true}`, "manager")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Fatalf("Message calls = %d, want 1", len(rt.sent))
	}
	got := rt.sent[0]
	if got.To != "agent:scratch" || got.Project != "/lever/groves/scratch" || !got.Interrupt || got.Body != "go" {
		t.Fatalf("bad MsgOpts: %+v", got)
	}
}

func TestMsgSend_groveToUser(t *testing.T) {
	b, rt, _ := newMsgTestBroker(true)
	rec := callGrove(t, b, "/msg/send", `{"to":"user:manager"}`, "scratch")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Fatalf("Message calls = %d, want 1", len(rt.sent))
	}
	got := rt.sent[0]
	if got.To != "agent:assistant" || got.Project != "/lever" {
		t.Fatalf("bad MsgOpts: %+v", got)
	}
}

func TestMsgSend_groveToGroveDisabled(t *testing.T) {
	b, rt, audit := newMsgTestBroker(false)
	rec := callGrove(t, b, "/msg/send", `{"to":"worker"}`, "scratch")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(rt.sent) != 0 {
		t.Fatalf("Message calls = %d, want 0 (denied before dispatch)", len(rt.sent))
	}
	if !strings.Contains(audit.String(), "deny") || !strings.Contains(audit.String(), "worker") {
		t.Fatalf("deny audit line missing recipient: %s", audit.String())
	}
}

func TestMsgSend_unknownCaller(t *testing.T) {
	b, rt, _ := newMsgTestBroker(true)
	rec := callGrove(t, b, "/msg/send", `{"to":"scratch"}`, "mallory")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(rt.sent) != 0 {
		t.Fatal("Message must not be called for an unknown caller")
	}
}

func TestMsgList_managerReadsGrove(t *testing.T) {
	b, rt, _ := newMsgTestBroker(true)
	rt.events = []scion.Event{{"id": "1", "type": "test"}}
	rec := callGrove(t, b, "/msg/list", `{"grove":"scratch"}`, "manager")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rt.inboxProject != "/lever/groves/scratch" {
		t.Fatalf("inboxProject = %q, want /lever/groves/scratch", rt.inboxProject)
	}
	var out msgListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0]["id"] != "1" {
		t.Fatalf("events not round-tripped: %+v", out.Events)
	}
}

func TestMsgList_groveForbiddenOtherGrove(t *testing.T) {
	b, _, _ := newMsgTestBroker(true)
	rec := callGrove(t, b, "/msg/list", `{"grove":"worker"}`, "scratch")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestMsgNilRuntime_returns502 proves both handlers return 502 (not a panic)
// when the scion runtime is unwired, and only after authn/authz has run.
func TestMsgNilRuntime_returns502(t *testing.T) {
	b := New(Config{
		ManagerIdentity: "assistant",
		ManagerProject:  "/lever",
		GroveToGrove:    true,
		Groves: []GroveSpec{
			{Name: "scratch", JailProject: "/lever/groves/scratch"},
		},
		Runtime:  nil,
		Registry: registry.New(),
	})

	rec := callGrove(t, b, "/msg/send", `{"to":"scratch","body":"go"}`, "assistant")
	if rec.Code != 502 {
		t.Fatalf("/msg/send nil-runtime: status = %d, want 502", rec.Code)
	}

	req2 := httptest.NewRequest("POST", "/msg/list", strings.NewReader(`{"grove":"scratch"}`))
	req2.TLS = fakeTLSWithCN("assistant")
	w2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(w2, req2)
	if w2.Code != 502 {
		t.Fatalf("/msg/list nil-runtime: status = %d, want 502", w2.Code)
	}
}

// TestMsgBadBody_returns400 posts invalid JSON to each handler through the real
// mux: 400 on the wire, "bad body" in the audit log.
func TestMsgBadBody_returns400(t *testing.T) {
	for _, path := range []string{"/msg/send", "/msg/list"} {
		t.Run(path, func(t *testing.T) {
			b, rt, audit := newMsgTestBroker(true)
			rec := callGrove(t, b, path, `{not json`, "manager")
			if rec.Code != 400 {
				t.Fatalf("%s status = %d, want 400", path, rec.Code)
			}
			if !strings.Contains(audit.String(), "bad body") {
				t.Fatalf("%s audit missing \"bad body\": %s", path, audit.String())
			}
			if len(rt.sent) != 0 || rt.inboxProject != "" {
				t.Fatalf("%s runtime must not be called on decode failure", path)
			}
		})
	}
}

// TestMsgRuntimeError_genericBody proves a runtime failure returns 502 with a
// GENERIC body (package convention, grove.go): the scion error text — which can
// echo the recipient/message body from argv — must appear only in the audit log.
func TestMsgRuntimeError_genericBody(t *testing.T) {
	secret := "scion: message secret-body failed"

	b, rt, audit := newMsgTestBroker(true)
	rt.sendErr = errors.New(secret)
	rec := callGrove(t, b, "/msg/send", `{"to":"scratch","body":"go"}`, "manager")
	if rec.Code != 502 {
		t.Fatalf("/msg/send status = %d, want 502", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "runtime error" {
		t.Fatalf("/msg/send body = %q, want generic \"runtime error\"", rec.Body.String())
	}
	if !strings.Contains(audit.String(), secret) {
		t.Fatalf("/msg/send audit missing error detail: %s", audit.String())
	}

	b2, rt2, audit2 := newMsgTestBroker(true)
	rt2.inboxErr = errors.New(secret)
	rec2 := callGrove(t, b2, "/msg/list", `{"grove":"scratch"}`, "manager")
	if rec2.Code != 502 {
		t.Fatalf("/msg/list status = %d, want 502", rec2.Code)
	}
	if strings.TrimSpace(rec2.Body.String()) != "runtime error" {
		t.Fatalf("/msg/list body = %q, want generic \"runtime error\"", rec2.Body.String())
	}
	if !strings.Contains(audit2.String(), secret) {
		t.Fatalf("/msg/list audit missing error detail: %s", audit2.String())
	}
}

func TestMsgSend_deniesRevokedCaller(t *testing.T) {
	b, rt, audit := newMsgTestBroker(true)
	b.Revoke("scratch")
	rec := callGrove(t, b, "/msg/send", `{"to":"user:manager","body":"steer"}`, "scratch")
	if rec.Code != 403 {
		t.Fatalf("revoked sender: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 0 {
		t.Fatalf("revoked sender must not deliver: sent = %d", len(rt.sent))
	}
	if !strings.Contains(audit.String(), "revoked") {
		t.Fatalf("deny must audit 'revoked', got: %s", audit.String())
	}
}

func TestMsgList_deniesRevokedCaller(t *testing.T) {
	b, _, _ := newMsgTestBroker(true)
	b.Revoke("manager")
	rec := callGrove(t, b, "/msg/list", `{"all":false}`, "manager")
	if rec.Code != 403 {
		t.Fatalf("revoked msg list: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestGroveList_deniesRevokedManager(t *testing.T) {
	b, _, _ := newMsgTestBroker(true)
	b.Revoke("manager")
	rec := callGrove(t, b, "/grove/list", `{}`, "manager")
	if rec.Code != 403 {
		t.Fatalf("revoked grove list: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}
