package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

func TestResolveMsgTarget(t *testing.T) {
	cases := []struct {
		name, caller, to string
		g2g              bool
		wantTo, wantProj string
		wantErr          bool
		wantErrIs        error // optional: the sentinel the deny must wrap
	}{
		{"manager to worker bare", "manager", "scratch", true, "agent:scratch", "/lever", false, nil},
		{"manager to worker prefixed", "manager", "agent:scratch", true, "agent:scratch", "/lever", false, nil},
		{"manager to manager by slug", "manager", "assistant", true, "agent:assistant", "/lever", false, nil},
		{"manager to manager slug prefixed", "manager", "agent:assistant", true, "agent:assistant", "/lever", false, nil},
		{"manager to manager by CN", "manager", "manager", true, "agent:assistant", "/lever", false, nil},
		{"manager to user alias+CN", "manager", "user:manager", true, "agent:assistant", "/lever", false, nil},
		{"manager to user slug", "manager", "user:assistant", true, "agent:assistant", "/lever", false, nil},
		{"manager to user other", "manager", "user:stephen", true, "", "", true, nil},
		{"manager to unknown worker", "manager", "nope", true, "", "", true, nil},
		{"worker to manager by slug", "scratch", "agent:assistant", true, "agent:assistant", "/lever", false, nil},
		{"worker to manager by CN", "scratch", "agent:manager", true, "agent:assistant", "/lever", false, nil},
		{"worker to user", "scratch", "user:manager", true, "agent:assistant", "/lever", false, nil},
		{"worker to worker allowed", "scratch", "worker", true, "agent:worker", "/lever", false, nil},
		{"worker to worker disabled", "scratch", "worker", false, "", "", true, nil},
		{"worker to itself", "scratch", "scratch", true, "agent:scratch", "/lever", false, nil},
		{"unknown caller", "mallory", "assistant", true, "", "", true, nil},
		{"caller by slug is not an identity", "assistant", "scratch", true, "", "", true, nil},
		{"worker to unknown", "scratch", "nope", true, "", "", true, nil},
		// Bare prefixes are NOT the empty manager alias / empty agent name:
		// they must fall through to the unknown-recipient deny.
		{"bare user: prefix denied", "manager", "user:", true, "", "", true, errUnknownRecipient},
		{"bare agent: prefix denied", "manager", "agent:", true, "", "", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := msgBroker(t, c.g2g).resolveMsgTarget(c.caller, c.to)
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErrIs != nil && !errors.Is(err, c.wantErrIs) {
				t.Fatalf("err = %v, want %v", err, c.wantErrIs)
			}
			if err == nil && (tgt.scionTo != c.wantTo || tgt.project != c.wantProj) {
				t.Fatalf("got (%q,%q), want (%q,%q)", tgt.scionTo, tgt.project, c.wantTo, c.wantProj)
			}
		})
	}
}

func TestResolveListSubject(t *testing.T) {
	cases := []struct {
		name, caller, worker string
		want                 string
		wantErr              bool
	}{
		// The subject is an agent SLUG. The manager's own is its scion slug, not
		// its cert CN — the hub knows it only by the slug.
		{"manager own inbox", "manager", "", "assistant", false},
		{"manager reads worker", "manager", "scratch", "scratch", false},
		{"manager unknown worker", "manager", "nope", "", true},
		{"worker own inbox", "scratch", "", "scratch", false},
		{"worker may not target others", "scratch", "worker", "", true},
		{"unknown caller", "mallory", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := msgBroker(t, true).resolveListSubject(c.caller, c.worker)
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("subject = %q, want %q", got, c.want)
			}
		})
	}
}

// fakeMsgRuntime embeds the package's existing fakeRuntime (via the WorkerRuntime
// interface field) for lifecycle methods it never exercises, and overrides
// Message/Inbox to capture what the msg handlers pass through.
type fakeMsgRuntime struct {
	WorkerRuntime
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

func TestMsgSend_managerToWorker(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	rec := callWorker(t, b, "/msg/send", `{"to":"scratch","body":"go","interrupt":true}`, "manager")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.sent) != 1 {
		t.Fatalf("Message calls = %d, want 1", len(rt.sent))
	}
	got := rt.sent[0]
	if got.To != "agent:scratch" || got.Project != "/lever" || !got.Interrupt || got.Body != "go" {
		t.Fatalf("bad MsgOpts: %+v", got)
	}
}

func TestMsgSend_workerToUser(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	rec := callWorker(t, b, "/msg/send", `{"to":"user:manager"}`, "scratch")
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

func TestMsgSend_workerToWorkerDisabled(t *testing.T) {
	b, rt, audit := newMsgTestBroker(t, false)
	rec := callWorker(t, b, "/msg/send", `{"to":"worker"}`, "scratch")
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

// TestMsgSendDenyLeaksReason proves the /msg/send policy-resolution deny
// (resolveMsgTarget) returns its specific reason as the HTTP body — not a
// bare "forbidden" — so agents can self-correct instead of reverse-engineering
// routable addresses by trial and error. Contrast with
// TestMsgRuntimeError_genericBody, whose scion-runtime branch MUST stay opaque.
func TestMsgSendDenyLeaksReason(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	rec := callWorker(t, b, "/msg/send", `{"to":"user:stephen"}`, "manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(rt.sent) != 0 {
		t.Fatal("Message must not be called on a resolve deny")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not broker-routable") {
		t.Fatalf("deny body should carry the resolve reason, got %q", body)
	}
}

// TestMsgListDenyLeaksReason is the /msg/list analogue: resolveListProject's
// reason must reach the HTTP body.
func TestMsgListDenyLeaksReason(t *testing.T) {
	b, _, _ := newMsgTestBroker(t, true)
	rec := callWorker(t, b, "/msg/list", `{"worker":"worker"}`, "scratch")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "a worker may only read its own inbox") {
		t.Fatalf("deny body should carry the resolve reason, got %q", body)
	}
}

func TestMsgSend_unknownCaller(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	rec := callWorker(t, b, "/msg/send", `{"to":"scratch"}`, "mallory")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(rt.sent) != 0 {
		t.Fatal("Message must not be called for an unknown caller")
	}
}

func TestMsgList_managerReadsWorker(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	withAgentIDs(b)
	// The event must be ATTRIBUTED to scratch: /msg/list now returns only the
	// events the hub attributes to the subject agent.
	rt.events = []scion.Event{{"id": "1", "type": "test", "agentId": "id-scratch"}}
	rec := callWorker(t, b, "/msg/list", `{"worker":"scratch"}`, "manager")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rt.inboxProject != "/lever" {
		t.Fatalf("inboxProject = %q, want /lever (the instance project)", rt.inboxProject)
	}
	var out wire.MsgListResponse[scion.Event]
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0]["id"] != "1" {
		t.Fatalf("events not round-tripped: %+v", out.Events)
	}
}

func TestMsgList_workerForbiddenOtherWorker(t *testing.T) {
	b, _, _ := newMsgTestBroker(t, true)
	rec := callWorker(t, b, "/msg/list", `{"worker":"worker"}`, "scratch")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestMsgNilRuntime_returns502 proves both handlers return 502 (not a panic)
// when the scion runtime is unwired, and only after authn/authz has run.
func TestMsgNilRuntime_returns502(t *testing.T) {
	b := New(testConfig(t, withManager("assistant", ""),
		withRuntime(nil, WorkerSpec{Name: "scratch", WorkspaceSubdir: "workers/scratch"}),
		func(c *Config) { c.Dispatch.WorkerToWorker = true }))

	rec := callWorker(t, b, "/msg/send", `{"to":"scratch","body":"go"}`, "assistant")
	if rec.Code != 502 {
		t.Fatalf("/msg/send nil-runtime: status = %d, want 502", rec.Code)
	}

	req2 := httptest.NewRequest("POST", "/msg/list", strings.NewReader(`{"worker":"scratch"}`))
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
			b, rt, audit := newMsgTestBroker(t, true)
			rec := callWorker(t, b, path, `{not json`, "manager")
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
// GENERIC body (package convention, worker.go): the scion error text — which can
// echo the recipient/message body from argv — must appear only in the audit log.
func TestMsgRuntimeError_genericBody(t *testing.T) {
	secret := "scion: message secret-body failed"

	b, rt, audit := newMsgTestBroker(t, true)
	rt.sendErr = errors.New(secret)
	rec := callWorker(t, b, "/msg/send", `{"to":"scratch","body":"go"}`, "manager")
	if rec.Code != 502 {
		t.Fatalf("/msg/send status = %d, want 502", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "runtime error" {
		t.Fatalf("/msg/send body = %q, want generic \"runtime error\"", rec.Body.String())
	}
	if !strings.Contains(audit.String(), secret) {
		t.Fatalf("/msg/send audit missing error detail: %s", audit.String())
	}

	b2, rt2, audit2 := newMsgTestBroker(t, true)
	withAgentIDs(b2)
	rt2.inboxErr = errors.New(secret)
	rec2 := callWorker(t, b2, "/msg/list", `{"worker":"scratch"}`, "manager")
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
	b, rt, audit := newMsgTestBroker(t, true)
	b.Revoke("scratch")
	rec := callWorker(t, b, "/msg/send", `{"to":"user:manager","body":"steer"}`, "scratch")
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

// assertRevokedManagerDenied revokes the manager and pins that path answers 403.
func assertRevokedManagerDenied(t *testing.T, path, body string) {
	t.Helper()
	b, _, _ := newMsgTestBroker(t, true)
	b.Revoke("manager")
	rec := callWorker(t, b, path, body, "manager")
	if rec.Code != 403 {
		t.Fatalf("revoked %s: status = %d, want 403 (%s)", path, rec.Code, rec.Body.String())
	}
}

func TestMsgList_deniesRevokedCaller(t *testing.T) {
	assertRevokedManagerDenied(t, "/msg/list", `{"all":false}`)
}

func TestWorkerList_deniesRevokedManager(t *testing.T) {
	assertRevokedManagerDenied(t, "/worker/list", `{}`)
}

// The hub scopes `scion notifications` to the authenticated USER, and lever
// always authenticates as the host controller PAT — so the raw feed carries
// every agent's events. Handing that to a caller breaks the isolation the rest
// of the model enforces, so /msg/list must cut it down to the one agent whose
// inbox the caller may read.
func msgListEvents(t *testing.T, b *Broker, rt *fakeMsgRuntime, body, cn string) []scion.Event {
	t.Helper()
	rec := callWorker(t, b, "/msg/list", body, cn)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var resp wire.MsgListResponse[scion.Event]
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Events
}

// fleetEvents is one event per agent, as the controller's feed really returns.
func fleetEvents() []scion.Event {
	return []scion.Event{
		{"id": "e1", "agentId": "id-assistant", "message": "manager event"},
		{"id": "e2", "agentId": "id-scratch", "message": "scratch event"},
		{"id": "e3", "agentId": "id-worker", "message": "worker event"},
		{"id": "e4", "message": "unattributed event"},
	}
}

func withAgentIDs(b *Broker) {
	b.resolveAgentID = func(_ context.Context, slug string) (string, error) {
		switch slug {
		case "assistant", "scratch", "worker":
			return "id-" + slug, nil
		}
		return "", fmt.Errorf("unknown agent %q", slug)
	}
}

// assertFleetCut lists the fleet feed as cn with body and pins that exactly
// one event — wantID — survives the cut.
func assertFleetCut(t *testing.T, body, cn, wantID, why string) {
	t.Helper()
	b, rt, _ := newMsgTestBroker(t, true)
	withAgentIDs(b)
	rt.events = fleetEvents()
	got := msgListEvents(t, b, rt, body, cn)
	if len(got) != 1 || got[0]["id"] != wantID {
		t.Fatalf("%s, got %+v", why, got)
	}
}

func TestMsgList_workerSeesOnlyItsOwnEvents(t *testing.T) {
	assertFleetCut(t, `{"all":true}`, "scratch", "e2", "a worker must see only its own events")
}

func TestMsgList_managerSeesOnlyItsOwnEventsByDefault(t *testing.T) {
	assertFleetCut(t, `{"all":true}`, "manager", "e1", "the manager's default inbox is its own")
}

// The documented manager-only selector must now actually select.
func TestMsgList_managerWorkerSelectorSelects(t *testing.T) {
	assertFleetCut(t, `{"all":true,"worker":"worker"}`, "manager", "e3", "--worker must select that worker's events")
}

// An event lever cannot attribute is dropped, not passed through: attribution is
// the whole basis of the cut.
func TestMsgList_dropsUnattributedEvents(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	withAgentIDs(b)
	rt.events = []scion.Event{{"id": "e4", "message": "unattributed"}}
	if got := msgListEvents(t, b, rt, `{"all":true}`, "scratch"); len(got) != 0 {
		t.Fatalf("unattributed events must be dropped, got %+v", got)
	}
}

// Without a way to attribute events, returning the raw feed would be the leak.
func TestMsgList_failsClosedWhenAgentIDUnresolvable(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	b.resolveAgentID = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("hub unreachable")
	}
	rt.events = fleetEvents()
	rec := callWorker(t, b, "/msg/list", `{"all":true}`, "scratch")
	if rec.Code == 200 {
		t.Fatalf("must fail closed, got 200: %s", rec.Body.String())
	}
}

func TestMsgList_failsClosedWhenResolverUnwired(t *testing.T) {
	b, rt, _ := newMsgTestBroker(t, true)
	rt.events = fleetEvents()
	rec := callWorker(t, b, "/msg/list", `{"all":true}`, "scratch")
	if rec.Code == 200 {
		t.Fatalf("an unwired resolver must not fall back to the fleet feed, got 200: %s", rec.Body.String())
	}
}

func TestIdentity(t *testing.T) {
	b := msgBroker(t, true) // manager CN "manager", slug "assistant", workers scratch/worker
	cases := []struct {
		name              string
		wantCN, wantSlug  string
		wantManager, want bool
	}{
		{"manager", "manager", "assistant", true, true},
		{"assistant", "manager", "assistant", true, true},
		{"scratch", "scratch", "scratch", false, true},
		{"nope", "", "", false, false},
		{"", "", "", false, false},
	}
	for _, tc := range cases {
		cn, slug, isManager, ok := b.identity(tc.name)
		if cn != tc.wantCN || slug != tc.wantSlug || isManager != tc.wantManager || ok != tc.want {
			t.Fatalf("identity(%q) = (%q, %q, %v, %v), want (%q, %q, %v, %v)",
				tc.name, cn, slug, isManager, ok, tc.wantCN, tc.wantSlug, tc.wantManager, tc.want)
		}
	}
}
