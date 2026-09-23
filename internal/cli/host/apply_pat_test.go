package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// seedPAT persists a token AND a record that vouches for it (current scopes,
// a far-off expiry), which is what an instance minted by this lever looks like.
func seedPAT(t *testing.T, st state.State, which, token string) {
	t.Helper()
	far := time.Now().Add(300 * 24 * time.Hour)
	switch which {
	case "controller":
		if err := st.SaveControllerPAT(token); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveControllerPATRecord(state.PATRecord{ID: "old-" + token, Requested: controllerPATScopes(), MintedAt: time.Now(), ExpiresAt: far}); err != nil {
			t.Fatal(err)
		}
	case "remote":
		if err := st.SaveRemotePAT(token); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveRemotePATRecord(state.PATRecord{ID: "old-" + token, Requested: remotePATScopes(), MintedAt: time.Now(), ExpiresAt: far}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("seedPAT: unknown token %q", which)
	}
}

func TestPATMintReason(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	want := []string{"a:b", "c:d"}
	ok := state.PATRecord{Requested: []string{"c:d", "a:b"}, ExpiresAt: now.Add(200 * 24 * time.Hour)}
	for _, tc := range []struct {
		name   string
		tok    string
		rec    state.PATRecord
		found  bool
		reason string // substring; "" means no mint
	}{
		{"absent token", "", ok, true, "no token"},
		{"fine, scope order irrelevant", "tok", ok, true, ""},
		{"no record", "tok", state.PATRecord{}, false, "no record"},
		{"scope drift", "tok", state.PATRecord{Requested: []string{"a:b"}, ExpiresAt: ok.ExpiresAt}, true, "scopes"},
		{"expires inside the renew window", "tok", state.PATRecord{Requested: want, ExpiresAt: now.Add(patRenewWindow - time.Hour)}, true, "expire"},
		{"already expired", "tok", state.PATRecord{Requested: want, ExpiresAt: now.Add(-time.Hour)}, true, "expire"},
		{"unknown expiry is not a reason", "tok", state.PATRecord{Requested: want}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := patMintReason(tc.tok, tc.rec, tc.found, want, now)
			if tc.reason == "" && got != "" {
				t.Fatalf("want no mint, got reason %q", got)
			}
			if tc.reason != "" && !strings.Contains(got, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", got, tc.reason)
			}
		})
	}
}

// The record is what lets a later apply or doctor judge the token without a
// hub call, so a mint must leave one behind that says what was asked, what
// the hub granted, and when it dies.
func TestEnsureControllerPATWritesTheRecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	f.Script(argvScionTokenCreate, proc.Result{Stdout: "Created access token: lever-controller\n" +
		"  ID:      a8bf56c4-383e-4e6a-ac2c-db7fe509c688\n" +
		"  Scopes:  agent:create, agent:attach, project:read\n" +
		"  Expires: 2027-09-09T10:08:14Z\n\nToken: scion_pat_new\n"})
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{}); err != nil {
		t.Fatal(err)
	}
	rec, found, err := st.LoadControllerPATRecord()
	if err != nil || !found {
		t.Fatalf("record: found=%v err=%v", found, err)
	}
	if rec.ID != "a8bf56c4-383e-4e6a-ac2c-db7fe509c688" {
		t.Errorf("record id = %q", rec.ID)
	}
	if strings.Join(rec.Requested, ",") != strings.Join(controllerPATScopes(), ",") {
		t.Errorf("requested = %v, want %v", rec.Requested, controllerPATScopes())
	}
	if strings.Join(rec.Granted, ",") != "agent:create,agent:attach,project:read" {
		t.Errorf("granted = %v", rec.Granted)
	}
	if rec.ExpiresAt.Year() != 2027 || rec.MintedAt.IsZero() {
		t.Errorf("times = minted %v expires %v", rec.MintedAt, rec.ExpiresAt)
	}
}

// An upgrade that changes the scope set must re-mint, and the token it
// replaces must not stay valid in the hub: it is revoked in the same window,
// by the id the record kept, AFTER the new one is persisted.
func TestEnsureControllerPATRemintsOnScopeDriftAndRevokesTheOld(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-old")
	if err := st.SaveControllerPATRecord(state.PATRecord{ID: "old-id", Requested: []string{"agent:manage"}, ExpiresAt: time.Now().Add(300 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("pat-new")
	f.Script(argvScionTokenRevoke, proc.Result{})
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{}); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.LoadControllerPAT(); tok != "pat-new" {
		t.Fatalf("controller PAT = %q, want the re-minted pat-new", tok)
	}
	iCreate := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionTokenCreate) })
	iRevoke := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionTokenRevoke+" old-id") })
	if iCreate < 0 || iRevoke < 0 || iRevoke < iCreate {
		t.Fatalf("want create then revoke old-id; create=%d revoke=%d calls=%+v", iCreate, iRevoke, f.Calls)
	}
	rec, _, _ := st.LoadControllerPATRecord()
	if rec.ID == "old-id" {
		t.Fatal("record still describes the revoked token")
	}
}

// A token minted by a lever that predates records is the live case on every
// existing instance: it was minted without --expires and without
// agent:message. It is re-minted once; there is no id to revoke, and that
// must not be an error.
func TestEnsureControllerPATRemintsALegacyTokenWithoutARecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := st.SaveControllerPAT("pat-legacy"); err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("pat-new")
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{}); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.LoadControllerPAT(); tok != "pat-new" {
		t.Fatalf("controller PAT = %q, want pat-new", tok)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionTokenRevoke) }); n != 0 {
		t.Fatalf("revoke calls = %d, want 0 (no id known)", n)
	}
}

func TestEnsureControllerPATRemintsNearExpiry(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-old")
	now := time.Now()
	if err := st.SaveControllerPATRecord(state.PATRecord{Requested: controllerPATScopes(), ExpiresAt: now.Add(5 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("pat-new")
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.LoadControllerPAT(); tok != "pat-new" {
		t.Fatalf("controller PAT = %q, want pat-new", tok)
	}
}

// Revoking the superseded token is best-effort: the new token is already
// persisted and working, and failing apply here would leave the operator
// worse off than a stale-but-unreferenced token does. It is reported.
func TestEnsureControllerPATRevokeFailureIsAWarning(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-old")
	if err := st.SaveControllerPATRecord(state.PATRecord{ID: "old-id", Requested: []string{"agent:manage"}, ExpiresAt: time.Now().Add(300 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("pat-new") // revoke unscripted => fails
	var warned []string
	warn := func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{Warn: warn}); err != nil {
		t.Fatalf("a failed revoke must not fail apply: %v", err)
	}
	if len(warned) == 0 || !strings.Contains(strings.Join(warned, "\n"), "old-id") {
		t.Fatalf("want a warning naming the token that could not be revoked, got %q", warned)
	}
}

// The remote token follows the same policy in the same window.
func TestEnsurePATsRemintsRemoteOnDriftOnly(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller-existing")
	seedPAT(t, st, "remote", "pat-remote-old")
	if err := st.SaveRemotePATRecord(state.PATRecord{ID: "old-remote", Requested: []string{"agent:read"}, ExpiresAt: time.Now().Add(300 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-remote", "pat-remote-new")
	f.Script(argvScionTokenRevoke, proc.Result{})
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{Enabled: true}, patMintOpts{AdminHub: newFakeAdminHub()}); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionTokenCreate) }); n != 1 {
		t.Fatalf("token create calls = %d, want 1 (remote only): %+v", n, f.Calls)
	}
	if ctok, _ := st.LoadControllerPAT(); ctok != "pat-controller-existing" {
		t.Fatalf("controller PAT changed to %q", ctok)
	}
	if rtok, _ := st.LoadRemotePAT(); rtok != "pat-remote-new" {
		t.Fatalf("remote PAT = %q, want pat-remote-new", rtok)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionTokenRevoke+" old-remote") }); n != 1 {
		t.Fatalf("revoke old-remote calls = %d, want 1", n)
	}
}

// ceilingHub is a hubapi transport that answers the ceiling read/write path
// and records whether it was asked at all.
type ceilingHub struct{ asked bool }

func (h *ceilingHub) Do(context.Context, string, string) (int, []byte, error) {
	h.asked = true
	return 0, nil, errors.New("unexpected hub call")
}

func (h *ceilingHub) DoBody(ctx context.Context, m, p string, _ []byte) (int, []byte, error) {
	return h.Do(ctx, m, p)
}

// A pre-roles scion has no ceiling setting (it landed with scion#1089), so
// the step must skip it — not fail apply on a settings write the hub cannot
// take. A probe that cannot answer fails closed, exactly as the --role stamp
// does in scion.Client.Start.
func TestEnsureAgentRoleCeilingIsGatedOnTheRolesProbe(t *testing.T) {
	h := &ceilingHub{}
	no := func(context.Context) (bool, error) { return false, nil }
	if err := ensureAgentRoleCeiling(context.Background(), no, &hubapi.Client{T: h}, "lever", "baseline"); err != nil {
		t.Fatalf("pre-roles scion: want skip, got %v", err)
	}
	if h.asked {
		t.Fatal("the hub must not be asked when the scion has no roles")
	}
	broken := func(context.Context) (bool, error) { return false, errors.New("exec: scion: not found") }
	if err := ensureAgentRoleCeiling(context.Background(), broken, &hubapi.Client{T: h}, "lever", "baseline"); err == nil {
		t.Fatal("a probe that cannot answer must fail closed")
	}
	yes := func(context.Context) (bool, error) { return true, nil }
	err := ensureAgentRoleCeiling(context.Background(), yes, &hubapi.Client{T: h}, "lever", "baseline")
	if err == nil || !h.asked {
		t.Fatalf("roles-aware scion: the hub must be asked and its failure surfaced; err=%v asked=%v", err, h.asked)
	}
}
