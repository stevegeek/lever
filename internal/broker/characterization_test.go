package broker

// characterization_test.go pins exact, previously-unasserted behavior of the
// broker security paths ahead of the Theme-A extractions
// (docs/superpowers/specs/2026-08-01-refactoring-plan.md, Session 2 /
// item A-TESTS). These are characterization tests: they assert what the code
// does TODAY — exact audit deny lines (A1), the full /request deny-detail
// string (A3), renew/enrol CSR proof-of-possession failures (A4), the
// directive admin channel's method/body validation (A2), and the reverse-proxy
// forwarding-header scrub (A7) — so a later refactor that silently changes an
// audit line, deny detail, status code, or scrub property fails loudly.

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// auditConfig returns testConfig with the audit log captured in a buffer, so
// tests can pin exact broker.decision lines.
func auditConfig(t *testing.T) (Config, *bytes.Buffer) {
	t.Helper()
	cfg := testConfig(t)
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	return cfg, &buf
}

// --- A1: exact deny audit lines on the copy-pasted authn+revocation preambles ---

// The directive consume/check handlers prefix their revoked-deny audit detail
// with the op name ("consume: revoked" / "check: revoked") — unlike most jail
// handlers, which audit a bare "revoked". Only the substring "revoked" was
// asserted before; a prefix regression would have passed silently.
func TestDirectiveConsumeCheckRevokedDenyAuditDetail(t *testing.T) {
	cases := []struct {
		path       string
		wantDetail string
	}{
		{"/directive/consume", `detail="consume: revoked"`},
		{"/directive/check", `detail="check: revoked"`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			cfg, audit := auditConfig(t)
			b := New(cfg)
			b.Revoke("manager")
			rec := callWorker(t, b, tc.path, `{"id":"x"}`, "manager")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(audit.String(), tc.wantDetail) {
				t.Fatalf("audit must contain %s, got: %s", tc.wantDetail, audit.String())
			}
		})
	}
}

// handleWorkerList audits its revoked-manager deny as "list: revoked" (its own
// prefixed spelling, matching /msg/list's recon rationale).
func TestWorkerListRevokedManagerDenyAuditDetail(t *testing.T) {
	cfg, audit := auditConfig(t)
	b := New(cfg)
	b.Revoke("manager")
	rec := callWorker(t, b, "/worker/list", `{}`, "manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(audit.String(), `detail="list: revoked"`) {
		t.Fatalf(`audit must contain detail="list: revoked", got: %s`, audit.String())
	}
}

// Pins the A1-sanctioned audit-line change on /worker/list: pre-refactor the
// handler merged authn-failure and wrong-CN into one branch that always
// audited "list: not the manager identity", dropping err.Error() on the
// certless path. Consolidated onto requireManager, a certless probe now
// audits caller="" with the op-prefixed RequireAgent error (consistent with
// every other jail handler), while a wrong-CN caller keeps the exact
// "list: not the manager identity" line.
func TestWorkerListCertlessAndWrongCNDenyAuditDetail(t *testing.T) {
	t.Run("certless", func(t *testing.T) {
		cfg, audit := auditConfig(t)
		b := New(cfg)
		r := httptest.NewRequest("POST", "/worker/list", strings.NewReader(`{}`)) // no client cert
		rec := httptest.NewRecorder()
		b.JailHandler().ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
		got := audit.String()
		if !strings.Contains(got, `caller=""`) {
			t.Fatalf(`certless deny must audit caller="", got: %s`, got)
		}
		if !strings.Contains(got, `detail="list: ca: request has no TLS state"`) {
			t.Fatalf(`certless deny must audit detail="list: ca: request has no TLS state", got: %s`, got)
		}
	})
	t.Run("wrong CN", func(t *testing.T) {
		cfg, audit := auditConfig(t)
		b := New(cfg)
		rec := callWorker(t, b, "/worker/list", `{}`, "worker")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(audit.String(), `detail="list: not the manager identity"`) {
			t.Fatalf(`wrong-CN deny must audit detail="list: not the manager identity", got: %s`, audit.String())
		}
	})
}

// A certless request to the directive consume/check routes must audit with an
// EMPTY caller and carry ca.RequireAgent's err.Error() (op-prefixed) in the
// detail — the forensics line for an unauthenticated probe.
func TestDirectiveCertlessDenyAuditsEmptyCallerAndError(t *testing.T) {
	cases := []struct {
		path       string
		wantDetail string
	}{
		{"/directive/consume", `detail="consume: ca: request has no TLS state"`},
		{"/directive/check", `detail="check: ca: request has no TLS state"`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			cfg, audit := auditConfig(t)
			b := New(cfg)
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"id":"x"}`)) // no client cert
			rec := httptest.NewRecorder()
			b.JailHandler().ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			got := audit.String()
			if !strings.Contains(got, `caller=""`) {
				t.Fatalf(`certless deny must audit caller="", got: %s`, got)
			}
			if !strings.Contains(got, tc.wantDetail) {
				t.Fatalf("certless deny must audit %s, got: %s", tc.wantDetail, got)
			}
		})
	}
}

// --- A3: the full /request deny-detail suffix string ---

// The policy deny detail must carry BOTH optional suffixes when a delegated,
// op-coerced request is denied: " coerced_to=<op>" and " bound_to=<agent>",
// inside the parenthesised suffix. Pinned byte-exact (HTTP body and audit
// line) — no prior test asserted coerced_to=, bound_to=, or the closing paren.
func TestRequestPolicyDenyDetailFullStringWithCoercionAndDelegation(t *testing.T) {
	cfg, audit := coarseConfig(t, false) // nobody holds the {utilities,*} grant
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "utilities", Op: "get_weather", BoundTo: "analyst", // manager delegating
	}))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	want := "policy: may not obtain/delegate (tool=utilities op=get_weather coerced_to=* bound_to=analyst)"
	if got := w.Body.String(); got != want+"\n" {
		t.Fatalf("deny body = %q, want %q", got, want+"\n")
	}
	if !strings.Contains(audit.String(), want) {
		t.Fatalf("audit must contain the full deny detail %q, got: %s", want, audit.String())
	}
}

// The unregistered-op deny path builds the same suffix: pin its full string
// with the bound_to suffix present (delegation grant exists, op unregistered).
func TestRequestUnregisteredOpDenyDetailFullString(t *testing.T) {
	cfg, audit := auditConfig(t)
	cfg.Rules.AllowDelegate("manager", "db", "drop", "worker") // grant for an op the registry lacks
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "db", Op: "drop", BoundTo: "worker",
	}))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	want := "unregistered op (tool=db op=drop bound_to=worker)"
	if got := w.Body.String(); got != want+"\n" {
		t.Fatalf("deny body = %q, want %q", got, want+"\n")
	}
	if !strings.Contains(audit.String(), want) {
		t.Fatalf("audit must contain the full deny detail %q, got: %s", want, audit.String())
	}
}

// --- A4: CSR proof-of-possession on /renew and garbage-PEM on both endpoints ---

// tamperCSRSignature flips the last byte of the CSR's DER (inside the
// signature value) and re-encodes, producing a parseable CSR whose
// self-signature no longer verifies.
func tamperCSRSignature(t *testing.T, csrPEM []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		t.Fatal("test CSR did not decode")
	}
	der := append([]byte(nil), blk.Bytes...)
	der[len(der)-1] ^= 0xFF
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// On /renew, CheckSignature is the ONLY proof-of-possession check
// (SignPublicKey does not re-verify, unlike enrol's ca.SignCSR). A refactor
// that dropped it would pass the rest of the suite — this test is the guard:
// a signature-tampered CSR must 403 and audit the verbatim
// "broker: CSR signature:" error prefix.
func TestRenewRejectsTamperedCSRSignature(t *testing.T) {
	cfg, audit := auditConfig(t)
	b := New(cfg)
	csr := tamperCSRSignature(t, makeCSRForCN(t, "worker"))
	body, _ := json.Marshal(RenewRequest{CSR: string(csr)})
	r := httptest.NewRequest("POST", "/renew", bytes.NewReader(body))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	b.handleRenew(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SECURITY: tampered-signature CSR renew: status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(audit.String(), "broker: CSR signature:") {
		t.Fatalf(`audit must carry the "broker: CSR signature:" deny detail, got: %s`, audit.String())
	}
}

// Garbage (non-PEM) CSR bytes must 403 on BOTH cert-issuing endpoints, with
// the verbatim "broker: invalid CSR PEM" detail in the audit ledger.
func TestRenewAndEnrolRejectGarbageCSRPEM(t *testing.T) {
	t.Run("renew", func(t *testing.T) {
		cfg, audit := auditConfig(t)
		b := New(cfg)
		body, _ := json.Marshal(RenewRequest{CSR: "not a pem"})
		r := httptest.NewRequest("POST", "/renew", bytes.NewReader(body))
		r.TLS = leafFor(t, b, "worker")
		w := httptest.NewRecorder()
		b.handleRenew(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("garbage-PEM renew: status = %d, want 403 (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(audit.String(), "broker: invalid CSR PEM") {
			t.Fatalf(`audit must carry "broker: invalid CSR PEM", got: %s`, audit.String())
		}
	})
	t.Run("enrol", func(t *testing.T) {
		cfg, audit := auditConfig(t)
		b := New(cfg)
		tk, err := b.tickets.Issue("worker", b.ticketTTL)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		b.handleEnrol(w, enrolReq(tk, []byte("not a pem")))
		if w.Code != http.StatusForbidden {
			t.Fatalf("garbage-PEM enrol: status = %d, want 403 (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(audit.String(), "broker: invalid CSR PEM") {
			t.Fatalf(`audit must carry "broker: invalid CSR PEM", got: %s`, audit.String())
		}
	})
}

// --- A2: directive admin channel — wrong method, resolve happy path, malformed bodies ---

// Every directive admin route must 405 a wrong-method request (when the
// channel is enabled). No 405 test existed anywhere; a wrong method constant
// in a future route wrapper would have passed the suite.
func TestDirectiveAdminRoutesReject405OnWrongMethod(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)
	cases := []struct{ path, method string }{
		{"/directive/send", http.MethodGet},
		{"/directive/resolve", http.MethodPost}, // the lone GET route
		{"/directive/list", http.MethodGet},
		{"/directive/revoke", http.MethodGet},
		{"/directive/selftest", http.MethodGet},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, "http://unix"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want 405", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// /directive/resolve happy path: the only broker-side positive coverage of
// the lone GET route — enrolled agent resolves to (cn, slug, generation).
func TestDirectiveResolveHappyPath(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager") // generation 0 -> 1 (as enrol would)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)
	resp, err := client.Get("http://unix/directive/resolve?agent=manager")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var got DirectiveResolveResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode resolve response: %v (%s)", err, body)
	}
	if got.CN != "manager" || got.Slug != "manager" || got.Generation != 1 {
		t.Fatalf("resolve = %+v, want {CN:manager Slug:manager Generation:1}", got)
	}
}

// send and selftest share a decode block (MaxBytesReader + two std-base64
// decodes): malformed JSON and undecodable base64 must both 400. Exactly the
// lines a future decodeSignedStatement extraction moves — unasserted before.
func TestDirectiveSendAndSelftestRejectMalformedBodies(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)
	for _, path := range []string{"/directive/send", "/directive/selftest"} {
		for name, body := range map[string]string{
			"malformed JSON": `{"statement":`,
			"bad base64":     `{"statement":"%%%","signature":"%%%"}`,
		} {
			resp, err := client.Post("http://unix"+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("%s %s: %v", path, name, err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s: status = %d, want 400 (%s)", path, name, resp.StatusCode, got)
			}
		}
	}
}

// --- A7: forwarding-header scrub in the two reverse-proxy Directors ---

// spoofedHeaders is what a jail agent could set to smuggle identity/session
// context upstream through the broker's reverse proxies.
func setSpoofedForwardingHeaders(h http.Header) {
	h.Set("Cookie", "session=evil")
	h.Set("X-Forwarded-For", "6.6.6.6")
	h.Set("X-Forwarded-Host", "evil.example")
	h.Set("X-Forwarded-Proto", "gopher")
}

type forwardedHeaders struct {
	cookie, xff, xfHost, xfProto string
}

func headerRecordingUpstream(t *testing.T, got *forwardedHeaders, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.cookie = r.Header.Get("Cookie")
		got.xff = r.Header.Get("X-Forwarded-For")
		got.xfHost = r.Header.Get("X-Forwarded-Host")
		got.xfProto = r.Header.Get("X-Forwarded-Proto")
		_, _ = io.WriteString(w, respBody)
	}))
}

// assertForwardingHeadersScrubbed pins the scrub property: Cookie,
// X-Forwarded-Host, and X-Forwarded-Proto never reach the upstream, and the
// inbound X-Forwarded-For chain is dropped. Note ReverseProxy re-adds the
// immediate client IP as X-Forwarded-For AFTER the Director runs, so the
// pinned property for XFF is "spoofed chain absent", not "header absent".
func assertForwardingHeadersScrubbed(t *testing.T, got forwardedHeaders) {
	t.Helper()
	if got.cookie != "" {
		t.Errorf("Cookie reached upstream: %q", got.cookie)
	}
	if strings.Contains(got.xff, "6.6.6.6") {
		t.Errorf("inbound X-Forwarded-For chain reached upstream: %q", got.xff)
	}
	if got.xfHost != "" {
		t.Errorf("X-Forwarded-Host reached upstream: %q", got.xfHost)
	}
	if got.xfProto != "" {
		t.Errorf("X-Forwarded-Proto reached upstream: %q", got.xfProto)
	}
}

func TestGatewayScrubsForwardingHeadersUpstream(t *testing.T) {
	var got forwardedHeaders
	up := headerRecordingUpstream(t, &got, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	setSpoofedForwardingHeaders(r.Header)
	w := httptest.NewRecorder()
	h, err := b.gatewayHandler("db")
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	assertForwardingHeadersScrubbed(t, got)
}

func TestLLMProxyScrubsForwardingHeadersUpstream(t *testing.T) {
	var got forwardedHeaders
	up := headerRecordingUpstream(t, &got, `{"ok":true}`)
	defer up.Close()
	b, caller := newTestBrokerForLLM(t, []byte("sk-REAL-KEY"), up.URL)
	tok := mintLLM(t, b.keys.Private, caller, b.MinEpoch())

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, caller, http.MethodPost, "/llm/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	setSpoofedForwardingHeaders(req.Header)
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertForwardingHeadersScrubbed(t, got)
}
