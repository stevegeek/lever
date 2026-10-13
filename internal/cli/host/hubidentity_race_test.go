package host

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/state"
)

// renameLoop models a hostile agent looping PUT /api/v1/system/identity
// (scion has no RBAC on it; any authenticated loopback caller may): right
// after every rename lever makes, the agent renames the dev row back.
type renameLoop struct {
	inner hiDoer
	h     *hiHub
}

func (r renameLoop) Do(ctx context.Context, m, p string) (int, []byte, error) {
	return r.DoBody(ctx, m, p, nil)
}

func (r renameLoop) DoBody(ctx context.Context, m, p string, b []byte) (int, []byte, error) {
	st, body, err := r.inner.DoBody(ctx, m, p, b)
	if m == http.MethodPut && p == "/api/v1/system/identity" {
		r.h.mu.Lock()
		if dev := r.h.user(devUserID); dev != nil {
			dev.Email = "agent-chosen@example.org"
		}
		r.h.mu.Unlock()
	}
	return st, body, err
}

// loopedStep is a hub-identity step on a hub where the agent's loop runs.
func (e *hiEnv) loopedStep() *hubIdentity {
	s := e.step()
	s.admin.T = renameLoop{inner: s.admin.T.(hiDoer), h: e.hub}
	s.pat.T = renameLoop{inner: s.pat.T.(hiDoer), h: e.hub}
	return s
}

// A rename loop on a migrated hub must not keep a removed contact live: the
// removal takes effect before any controller work, and an apply that mints
// nothing does not depend on the controller's address at all.
func TestHubIdentityRenameLoopCannotBlockContactRemoval(t *testing.T) {
	op, contact := "op@example.com", "c1@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	e.seedMigrated("dev@localhost", true)
	e.mustRun()
	c := e.hub.byEmail(contact)
	if c == nil || c.Status == "suspended" {
		t.Fatalf("setup: contact %+v", c)
	}
	e.hub.user(devUserID).Email = "agent-chosen@example.org"
	e.app.Remote.AllowedUsers = []config.RemoteUser{hiOperator(op)}
	for i := range 3 {
		if err := e.loopedStep().run(context.Background()); err != nil {
			t.Fatalf("apply %d under the rename loop with no mint due: %v", i, err)
		}
	}
	if c.Status != "suspended" || len(e.hub.boundTo(c.ID)) != 0 {
		t.Fatalf("removed contact: status %q bound %q", c.Status, e.hub.boundTo(c.ID))
	}
	if !hiAnyContains(e.warns, "renaming scion's dev user") {
		t.Fatalf("no warning about the rename: %q", e.warns)
	}
	if got := hiEventsWith(e.hub.events, "patch "+devUserID+" role=admin"); len(got) > 0 {
		t.Fatalf("the controller was promoted: %q", got)
	}
}

// With a mint due, the loop wins every bounded re-pin and assert, and the
// apply fails naming the cause; the removal has already taken effect and the
// controller is never promoted.
func TestHubIdentityRenameLoopDuringAMintFailsNamingTheCause(t *testing.T) {
	op, contact := "op@example.com", "c1@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	e.seedMigrated("dev@localhost", true)
	e.mustRun()
	c := e.hub.byEmail(contact)
	e.app.Remote.AllowedUsers = []config.RemoteUser{hiOperator(op)}
	if err := e.st.SaveControllerPATRecord(state.PATRecord{ID: "tok-x", Requested: []string{"agent:manage"},
		MintedAt: e.now, ExpiresAt: e.now.AddDate(0, 0, 300)}); err != nil {
		t.Fatal(err)
	}
	e.hub.user(devUserID).Email = "agent-chosen@example.org"
	n := len(e.hub.events)
	err := e.loopedStep().run(context.Background())
	if err == nil || !errors.Is(err, errControllerContested) || !strings.Contains(err.Error(), "already took effect") ||
		!strings.Contains(err.Error(), "lever stop") {
		t.Fatalf("run = %v, want the contested-controller error", err)
	}
	if c.Status != "suspended" {
		t.Fatalf("removed contact status %q", c.Status)
	}
	if got := hiEventsWith(e.hub.eventsFrom(n), "mint"); len(got) > 0 {
		t.Fatalf("minted under the loop: %q", got)
	}
	if got := hiEventsWith(e.hub.eventsFrom(n), "identity "+hubauth.ControllerEmail); len(got) > controllerAssertAttempts+1 {
		t.Fatalf("%d re-pins; the retry must be bounded", len(got))
	}
	// The agent stops; the next apply mints.
	e.mustRun()
	if got := hiEventsWith(e.hub.eventsFrom(n), "mint lever-controller"); len(got) != 1 {
		t.Fatalf("mints after the loop stopped: %q", got)
	}
}

// Suspension of a removed login comes before every controller step, so a
// failure there cannot leave the login active.
func TestHubIdentitySuspendsRemovedLoginsBeforeTheController(t *testing.T) {
	op, contact := "op@example.com", "c1@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	e.seedMigrated("dev@localhost", true)
	e.mustRun()
	c := e.hub.byEmail(contact)
	e.app.Remote.AllowedUsers = []config.RemoteUser{hiOperator(op)}
	e.hub.user(devUserID).Status = "suspended"
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "want an active member") {
		t.Fatalf("run = %v, want the controller check to fail", err)
	}
	if c.Status != "suspended" || len(e.hub.boundTo(c.ID)) != 0 {
		t.Fatalf("removed contact: status %q bound %q", c.Status, e.hub.boundTo(c.ID))
	}
}

// Remote access turned off suspends every recorded login first as well.
func TestHubIdentityRemoteOffSuspendsEveryLoginFirst(t *testing.T) {
	op := "op@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op)))
	e.mustRun()
	u := e.hub.byEmail(op)
	e.app = hiRemoteApp(false, hiOperator(op))
	e.hub.down = map[string]int{"GET /api/v1/auth/tokens": 500, "POST /api/v1/auth/tokens": 500}
	_ = e.run()
	if u.Status != "suspended" {
		t.Fatalf("operator status %q with remote access off", u.Status)
	}
}

// A reset of a hub whose database was kept revokes the tokens lever recorded
// before forgetting them, so none stays valid unrecorded.
func TestHubIdentityResetRevokesTheRecordedTokens(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(true))
	e.mustRun()
	ctlRec, _, _ := e.st.LoadControllerPATRecord()
	remRec, _, _ := e.st.LoadRemotePATRecord()
	e.reset = true
	e.mustRun()
	for _, id := range []string{ctlRec.ID, remRec.ID} {
		if e.hub.at("revoke "+id) < 0 {
			t.Fatalf("token %s was not revoked: %q", id, e.hub.events)
		}
	}
	if rec, _, _ := e.st.LoadControllerPATRecord(); rec.ID == ctlRec.ID {
		t.Fatal("the old controller token is still the recorded one")
	}
}

// On a re-bootstrap the tree's project marker must name the project lever
// registered before: the tree is the manager's workspace.
func TestHubIdentityRebootstrapRefusesARewrittenMarker(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.mustRun()
	e.jr.marker = "project-id: 0a0a0a0a-0000-4000-8000-000000000000\nproject-slug: agent-chosen\n"
	e.reset = true
	n := len(e.hub.events)
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "may have rewritten it") {
		t.Fatalf("run = %v", err)
	}
	if got := hiEventsWith(e.hub.eventsFrom(n), "register", "scion init", "revoke"); len(got) > 0 {
		t.Fatalf("went on with an agent-written marker: %q", got)
	}
	if _, found, _ := e.st.LoadHubIdentity(); !found {
		t.Fatal("the record was forgotten before the marker was checked")
	}
}

// With remote access off a failed revoke of the remote PAT is a warning, not
// a failed apply, and the record stays for the next apply to retry.
func TestHubIdentityRemoteOffRevokeFailureIsRetried(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(true))
	e.mustRun()
	rrec, _, _ := e.st.LoadRemotePATRecord()
	e.app = hiRemoteApp(false)
	e.hub.down = map[string]int{"POST /api/v1/auth/tokens/" + rrec.ID + "/revoke": 500}
	e.mustRun()
	if !hiAnyContains(e.warns, "stays valid until the next apply") {
		t.Fatalf("warns %q", e.warns)
	}
	if _, found, _ := e.st.LoadRemotePATRecord(); !found {
		t.Fatal("the record was dropped before the token was revoked")
	}
	e.hub.down = nil
	e.mustRun()
	if e.hub.at("revoke "+rrec.ID) < 0 {
		t.Fatal("the next apply did not revoke it")
	}
	if _, found, _ := e.st.LoadRemotePATRecord(); found {
		t.Fatal("the record outlived the revoke")
	}
}
