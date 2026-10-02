package broker

// directive_preview_test.go exercises /directive/preview (#17): the
// non-consuming read of a pending directive by its target. It pins that the
// gate is consume's gate (byte-identical opaque 404 for every miss), that a
// preview never consumes and never looks like a consume, the per-directive
// cap, and the audit lines.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
)

const previewPath = "/directive/preview"

// previewBroker is directiveTestBroker plus a captured broker audit and a
// directive audit log on disk.
func previewBroker(t *testing.T) (*Broker, *bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	logPath := filepath.Join(t.TempDir(), "directives.log")
	_, as := genOperatorKey(t)
	rt := &fakeDirectiveRuntime{}
	cfg := testConfig(t, withAudit(&buf), withRuntime(rt, WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker"}),
		func(c *Config) {
			c.Directives = DirectiveConfig{
				Verifier:   &opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"},
				InstanceID: "testinst",
				ExpiryMax:  24 * time.Hour,
				AuditPath:  logPath,
			}
		})
	return New(cfg), &buf, logPath
}

// dirAuditEvents returns the directive audit log's lines, decoded.
func dirAuditEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

type previewResp struct {
	ID                string          `json:"id"`
	Kind              string          `json:"kind"`
	Consumed          *bool           `json:"consumed"`
	Preview           opsig.Action    `json:"preview"`
	ExpiresAt         string          `json:"expires_at"`
	PreviewsRemaining int             `json:"previews_remaining"`
	Note              string          `json:"note"`
	Action            json.RawMessage `json:"action"`
	AdvisoryText      string          `json:"advisory_text"`
}

func decodePreview(t *testing.T, body []byte) previewResp {
	t.Helper()
	var p previewResp
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode preview: %v (%s)", err, body)
	}
	return p
}

// A preview returns the verified action in a shape that cannot pass for a
// consume result, changes no state but the count, and leaves the directive
// consumable exactly once.
func TestPreviewThenConsumeStillWorksExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action opsig.Action
	}{
		{"tool_call", toolCallAction("db", "read", `{"table":"A"}`)},
		{"approval", opsig.Action{Kind: "approval", Tool: "db", Op: "read", Args: json.RawMessage(`{"table":"A"}`), ArgBinding: "exact", Uses: 1}},
		{"instruction", instructionAction("hold outbound email")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, logPath := previewBroker(t)
			b.directives.BumpGeneration("manager")
			id := "11111111-2222-4333-8444-555555555701"
			st := directiveStatement(id, "manager", 1, tc.action)
			submitDirective(t, b, st)
			srv := jailServer(t, b)
			defer srv.Close()
			client := agentClient(t, b, signedCert(t, b, "manager"))

			// Two previews: idempotent content, only the count moves.
			var first previewResp
			for i := 1; i <= 2; i++ {
				status, body := postDirectiveID(t, client, srv.URL, previewPath, id)
				if status != http.StatusOK {
					t.Fatalf("preview %d = %d %s, want 200", i, status, body)
				}
				p := decodePreview(t, body)
				if p.ID != id || p.Kind != tc.action.Kind {
					t.Fatalf("preview %d = %+v", i, p)
				}
				if p.Consumed == nil || *p.Consumed {
					t.Fatalf("preview %d must carry consumed:false, got %s", i, body)
				}
				if len(p.Action) != 0 || p.AdvisoryText != "" {
					t.Fatalf("preview %d must not use the consume result fields: %s", i, body)
				}
				if !strings.Contains(p.Note, "NOT consumed") || !strings.Contains(p.Note, "directive_consume") {
					t.Fatalf("preview note = %q", p.Note)
				}
				if p.PreviewsRemaining != maxDirectivePreviews-i {
					t.Fatalf("preview %d previews_remaining = %d, want %d", i, p.PreviewsRemaining, maxDirectivePreviews-i)
				}
				gotExp, err := time.Parse(time.RFC3339, p.ExpiresAt)
				wantExp, _ := time.Parse(time.RFC3339, st.ExpiresAt)
				if err != nil || !gotExp.Equal(wantExp) {
					t.Fatalf("expires_at = %q, want the instant %q", p.ExpiresAt, st.ExpiresAt)
				}
				want, _ := json.Marshal(tc.action)
				got, _ := json.Marshal(p.Preview)
				if !bytes.Equal(want, got) {
					t.Fatalf("preview action = %s, want %s", got, want)
				}
				// The reply holds exactly these keys: no token, no grant.
				var keys map[string]json.RawMessage
				_ = json.Unmarshal(body, &keys)
				for k := range keys {
					switch k {
					case "id", "kind", "consumed", "preview", "expires_at", "previews_remaining", "note":
					default:
						t.Fatalf("unexpected key %q in preview reply %s", k, body)
					}
				}
				if i == 1 {
					first = p
				} else if got1, _ := json.Marshal(first.Preview); !bytes.Equal(got1, got) {
					t.Fatalf("preview content changed between calls")
				}
			}

			// Still active, still consumable — once.
			if state, ok := b.directives.Check(id, "manager", time.Now()); !ok || state != DirectiveActive {
				t.Fatalf("state after previews = %q %v, want active", state, ok)
			}
			if status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id); status != http.StatusOK {
				t.Fatalf("consume after preview = %d %s, want 200", status, body)
			}
			if status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id); status != http.StatusNotFound || string(body) != opaque404Body {
				t.Fatalf("second consume = %d %s, want opaque 404", status, body)
			}
			// A consumed directive no longer previews.
			if status, body := postDirectiveID(t, client, srv.URL, previewPath, id); status != http.StatusNotFound || string(body) != opaque404Body {
				t.Fatalf("preview after consume = %d %s, want opaque 404", status, body)
			}

			var events []string
			for _, e := range dirAuditEvents(t, logPath) {
				if e["id"] != id || e["caller"] != "manager" {
					t.Fatalf("audit line without the id/caller: %v", e)
				}
				ev := e["event"].(string)
				events = append(events, ev)
				if ev == "consumed" && e["previews"] != float64(2) {
					t.Fatalf("consumed line must record previews=2, got %v", e)
				}
				if ev == "previewed" && (e["kind"] != tc.action.Kind || e["previews"] == nil) {
					t.Fatalf("previewed line = %v", e)
				}
			}
			want := "previewed previewed consumed consume_denied preview_denied"
			if got := strings.Join(events, " "); got != want {
				t.Fatalf("directive audit events = %q, want %q", got, want)
			}
		})
	}
}

// Every miss is consume's miss: the same status and byte-identical body for a
// non-target, an unknown id, a stale generation, expired, revoked, consumed,
// before not_before, an empty id and a disabled channel.
func TestPreviewOpaque404ForEveryMiss(t *testing.T) {
	type setup func(t *testing.T, b *Broker, id string) (callerCN string)
	seed := func(st func(id string) opsig.Statement, after func(b *Broker, id string), caller string) setup {
		return func(t *testing.T, b *Broker, id string) string {
			b.directives.BumpGeneration("manager")
			submitDirective(t, b, st(id))
			if after != nil {
				after(b, id)
			}
			return caller
		}
	}
	active := func(id string) opsig.Statement {
		return directiveStatement(id, "manager", 1, toolCallAction("db", "read", `{"table":"A"}`))
	}
	cases := []struct {
		name  string
		setup setup
		// previewID overrides the id sent ("" = the seeded id).
		previewID string
		emptyID   bool
		// stillActive: the directive must be untouched and consumable by its
		// real target after the refused preview.
		stillActive bool
	}{
		{name: "non_target_caller", setup: seed(active, nil, "worker"), stillActive: true},
		{name: "unknown_id", setup: seed(active, nil, "manager"), previewID: "no-such-directive-id", stillActive: true},
		{name: "empty_id", setup: seed(active, nil, "manager"), emptyID: true, stillActive: true},
		{name: "stale_generation", setup: seed(active, func(b *Broker, _ string) { b.directives.BumpGeneration("manager") }, "manager")},
		{name: "expired", setup: seed(func(id string) opsig.Statement {
			return expiredStatement(id, "manager", 1, instructionAction("x"))
		}, nil, "manager")},
		{name: "revoked", setup: seed(active, func(b *Broker, id string) { b.directives.RevokeDirective(id) }, "manager")},
		{name: "already_consumed", setup: seed(active, func(b *Broker, id string) {
			b.directives.Consume(id, "manager", time.Now())
		}, "manager")},
		{name: "before_not_before", setup: seed(func(id string) opsig.Statement {
			st := active(id)
			st.NotBefore = time.Now().Add(5 * time.Minute).Format(time.RFC3339)
			return st
		}, nil, "manager"), stillActive: false},
		{name: "directives_disabled", setup: func(t *testing.T, b *Broker, id string) string {
			b.directives.BumpGeneration("manager")
			submitDirective(t, b, active(id))
			b.directiveVerifier = nil
			return "manager"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, logPath := previewBroker(t)
			id := "11111111-2222-4333-8444-555555555710"
			caller := tc.setup(t, b, id)
			srv := jailServer(t, b)
			defer srv.Close()
			client := agentClient(t, b, signedCert(t, b, caller))

			sendID := id
			if tc.previewID != "" {
				sendID = tc.previewID
			}
			if tc.emptyID {
				sendID = ""
			}
			status, body := postDirectiveID(t, client, srv.URL, previewPath, sendID)
			if status != http.StatusNotFound || string(body) != opaque404Body {
				t.Fatalf("preview = %d %s, want 404 %s", status, body, opaque404Body)
			}
			// The same call on consume gives the identical answer: preview is
			// no wider an oracle than consume.
			cStatus, cBody := postDirectiveID(t, client, srv.URL, "/directive/consume", sendID)
			if cStatus != status || !bytes.Equal(cBody, body) {
				t.Fatalf("consume = %d %s, preview = %d %s: must be identical", cStatus, cBody, status, body)
			}
			// A refused preview counts nothing and leaks nothing into the log.
			for _, r := range b.directives.List(time.Now()) {
				if r.Previews != 0 {
					t.Fatalf("refused preview changed the count: %+v", r)
				}
			}
			for _, e := range dirAuditEvents(t, logPath) {
				if e["event"] == "previewed" {
					t.Fatalf("refused preview audited as previewed: %v", e)
				}
			}
			if tc.stillActive {
				mgr := agentClient(t, b, signedCert(t, b, "manager"))
				if s, bd := postDirectiveID(t, mgr, srv.URL, "/directive/consume", id); s != http.StatusOK {
					t.Fatalf("target consume after a refused preview = %d %s, want 200", s, bd)
				}
			}
		})
	}
}

// A denied preview is audited with the caller and the id it tried.
func TestPreviewDeniedIsAudited(t *testing.T) {
	b, audit, logPath := previewBroker(t)
	b.directives.BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555720"
	submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))
	srv := jailServer(t, b)
	defer srv.Close()
	worker := agentClient(t, b, signedCert(t, b, "worker"))
	postDirectiveID(t, worker, srv.URL, previewPath, id)

	ev := dirAuditEvents(t, logPath)
	if len(ev) != 1 || ev[0]["event"] != "preview_denied" || ev[0]["caller"] != "worker" || ev[0]["id"] != id {
		t.Fatalf("directive audit = %v, want one preview_denied by worker", ev)
	}
	if !strings.Contains(audit.String(), "preview "+id+": no active match") {
		t.Fatalf("broker audit lacks the preview deny: %s", audit.String())
	}
}

// The cap: maxDirectivePreviews reads, then a distinct 429 for the target
// only. A capped directive is still consumable, and the cap tells a
// non-target nothing.
func TestPreviewCountCap(t *testing.T) {
	b, audit, logPath := previewBroker(t)
	b.directives.BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555730"
	submitDirective(t, b, directiveStatement(id, "manager", 1, toolCallAction("db", "read", `{"table":"A"}`)))
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	for i := 1; i <= maxDirectivePreviews; i++ {
		status, body := postDirectiveID(t, client, srv.URL, previewPath, id)
		if status != http.StatusOK {
			t.Fatalf("preview %d = %d %s, want 200", i, status, body)
		}
	}
	for i := 0; i < 2; i++ {
		status, body := postDirectiveID(t, client, srv.URL, previewPath, id)
		if status != http.StatusTooManyRequests || !strings.Contains(string(body), "preview limit reached") {
			t.Fatalf("preview over the cap = %d %s, want 429 preview limit reached", status, body)
		}
		if strings.Contains(string(body), "table") || strings.Contains(string(body), "db") {
			t.Fatalf("capped reply leaks content: %s", body)
		}
	}
	// A non-target still gets the opaque 404, not the cap answer.
	worker := agentClient(t, b, signedCert(t, b, "worker"))
	if status, body := postDirectiveID(t, worker, srv.URL, previewPath, id); status != http.StatusNotFound || string(body) != opaque404Body {
		t.Fatalf("non-target on a capped directive = %d %s, want opaque 404", status, body)
	}
	recs := b.directives.List(time.Now())
	if len(recs) != 1 || recs[0].Previews != maxDirectivePreviews || recs[0].State != DirectiveActive {
		t.Fatalf("record after cap = %+v", recs)
	}
	var capped int
	for _, e := range dirAuditEvents(t, logPath) {
		if e["event"] == "preview_capped" && e["caller"] == "manager" && e["id"] == id {
			capped++
		}
	}
	if capped != 2 {
		t.Fatalf("preview_capped audit lines = %d, want 2", capped)
	}
	if !strings.Contains(audit.String(), "preview limit reached") {
		t.Fatalf("broker audit lacks the cap deny: %s", audit.String())
	}
	if status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id); status != http.StatusOK {
		t.Fatalf("consume after the cap = %d %s, want 200", status, body)
	}
}

// A stored statement that no longer validates is never shown, and the failed
// preview does not burn the directive's state.
func TestPreviewRevalidatesStoredStatement(t *testing.T) {
	b, _, _ := previewBroker(t)
	b.directives.BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555740"
	now := time.Now()
	if err := b.directives.Submit(DirectiveRecord{
		ID: id, Statement: []byte(`{"v":1,"instance":"other"}`), TargetCN: "manager", TargetGen: 1,
		Kind: "instruction", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	if status, body := postDirectiveID(t, client, srv.URL, previewPath, id); status != http.StatusNotFound || string(body) != opaque404Body {
		t.Fatalf("preview of an invalid stored statement = %d %s, want opaque 404", status, body)
	}
	if state, _ := b.directives.Check(id, "manager", now); state != DirectiveActive {
		t.Fatalf("state = %q, want active (a preview never changes state)", state)
	}
}

func TestPreviewRevokedCallerDenied(t *testing.T) {
	b, _, _ := previewBroker(t)
	b.directives.BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555750"
	submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))
	b.Revoke("manager")
	srv := jailServer(t, b)
	defer srv.Close()
	if status, _ := postDirectiveID(t, agentClient(t, b, signedCert(t, b, "manager")), srv.URL, previewPath, id); status != http.StatusForbidden {
		t.Fatalf("revoked caller preview = %d, want 403", status)
	}
	if got := b.directives.List(time.Now())[0].Previews; got != 0 {
		t.Fatalf("revoked caller changed the preview count to %d", got)
	}
}

// Preview shares the per-CN directive rate window with consume and check.
func TestPreviewSharesDirectiveRateLimit(t *testing.T) {
	b, _, _ := previewBroker(t)
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))
	for i := 0; i < wantDirectiveRateLimit; i++ {
		path := previewPath
		if i%2 == 0 {
			path = "/directive/check"
		}
		if status, body := postDirectiveID(t, client, srv.URL, path, "nonexistent"); status == http.StatusTooManyRequests {
			t.Fatalf("call %d prematurely rate limited: %s", i+1, body)
		}
	}
	if status, body := postDirectiveID(t, client, srv.URL, previewPath, "nonexistent"); status != http.StatusTooManyRequests {
		t.Fatalf("call over the limit = %d %s, want 429", status, body)
	}
}

// ---- store ----

func TestStorePreviewGateMatchesConsumeAndNeverChangesState(t *testing.T) {
	now := time.Now()
	s := newStore(nil)
	s.BumpGeneration("mgr")
	s.BumpGeneration("other")
	if err := s.Submit(rec("d1", "mgr", 1, now), now); err != nil {
		t.Fatal(err)
	}
	misses := []struct {
		name, id, cn string
		at           time.Time
	}{
		{"unknown id", "nope", "mgr", now},
		{"wrong caller", "d1", "other", now},
		{"before not_before", "d1", "mgr", now.Add(-2 * time.Minute)},
		{"at expiry", "d1", "mgr", now.Add(10 * time.Minute)},
	}
	for _, m := range misses {
		if _, out := s.Preview(m.id, m.cn, m.at, 5); out != PreviewMiss {
			t.Fatalf("%s: outcome = %v, want PreviewMiss", m.name, out)
		}
	}
	got, out := s.Preview("d1", "mgr", now, 5)
	if out != PreviewOK || got.Previews != 1 || got.State != DirectiveActive || string(got.Statement) != "{}" {
		t.Fatalf("preview = %+v %v", got, out)
	}
	if _, ok := s.Consume("d1", "mgr", now); !ok {
		t.Fatal("consume after preview failed")
	}
	if _, out := s.Preview("d1", "mgr", now, 5); out != PreviewMiss {
		t.Fatalf("preview of a consumed directive = %v, want PreviewMiss", out)
	}

	// Revoked, and stale generation (which also invalidates).
	for _, id := range []string{"d2", "d3"} {
		if err := s.Submit(rec(id, "mgr", 1, now), now); err != nil {
			t.Fatal(err)
		}
	}
	s.RevokeDirective("d2")
	if _, out := s.Preview("d2", "mgr", now, 5); out != PreviewMiss {
		t.Fatalf("preview of a revoked directive = %v", out)
	}
	s.BumpGeneration("mgr")
	if _, out := s.Preview("d3", "mgr", now, 5); out != PreviewMiss {
		t.Fatalf("preview at a stale generation = %v", out)
	}
}

func TestStorePreviewCapPersistsAndFailsClosed(t *testing.T) {
	now := time.Now()
	var last DirectiveState
	fail := false
	s := newStore(func(st DirectiveState) error {
		if fail {
			return errors.New("disk full")
		}
		last = st
		return nil
	})
	s.BumpGeneration("mgr")
	if err := s.Submit(rec("d1", "mgr", 1, now), now); err != nil {
		t.Fatal(err)
	}
	if _, out := s.Preview("d1", "mgr", now, 2); out != PreviewOK {
		t.Fatalf("first preview = %v", out)
	}
	// A persist failure returns nothing and does not count.
	fail = true
	if got, out := s.Preview("d1", "mgr", now, 2); out != PreviewMiss || got.Statement != nil {
		t.Fatalf("preview on a persist failure = %+v %v, want PreviewMiss", got, out)
	}
	fail = false
	if got, out := s.Preview("d1", "mgr", now, 2); out != PreviewOK || got.Previews != 2 {
		t.Fatalf("second preview = %+v %v", got, out)
	}
	if _, out := s.Preview("d1", "mgr", now, 2); out != PreviewCapped {
		t.Fatalf("third preview = %v, want PreviewCapped", out)
	}
	// The count is durable: a store reloaded from the snapshot is still capped
	// for the target and still consumable.
	if last.Directives[0].Previews != 2 {
		t.Fatalf("persisted previews = %d, want 2", last.Directives[0].Previews)
	}
	s2 := newDirectiveStore(last, nil, nil)
	if _, out := s2.Preview("d1", "mgr", now, 2); out != PreviewCapped {
		t.Fatalf("preview after a reload = %v, want PreviewCapped", out)
	}
	if _, out := s2.Preview("d1", "other", now, 2); out != PreviewMiss {
		t.Fatalf("non-target on a capped directive = %v, want PreviewMiss", out)
	}
	if _, ok := s2.Consume("d1", "mgr", now); !ok {
		t.Fatal("a capped directive must still be consumable")
	}
}
