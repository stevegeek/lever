package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// seedPAT persists a token and a current record, as this lever mints them.
func seedPAT(t *testing.T, st state.State, which, token string) {
	t.Helper()
	far := time.Now().Add(300 * 24 * time.Hour)
	switch which {
	case "controller":
		if err := st.SaveControllerPAT(token); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveControllerPATRecord(state.PATRecord{ID: "old-" + token, Requested: controllerPATScopes(false, false), MintedAt: time.Now(), ExpiresAt: far}); err != nil {
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

// A mint must leave a record of what was asked, what the hub granted and the
// expiry, so apply and doctor can judge the token without a hub call.
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
	if strings.Join(rec.Requested, ",") != strings.Join(controllerPATScopes(false, false), ",") {
		t.Errorf("requested = %v, want %v", rec.Requested, controllerPATScopes(false, false))
	}
	if strings.Join(rec.Granted, ",") != "agent:create,agent:attach,project:read" {
		t.Errorf("granted = %v", rec.Granted)
	}
	if rec.ExpiresAt.Year() != 2027 || rec.MintedAt.IsZero() {
		t.Errorf("times = minted %v expires %v", rec.MintedAt, rec.ExpiresAt)
	}
}

// A scope-set change re-mints, and the replaced token is revoked in the same
// window, by the recorded id, after the new one is persisted.
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

// A token without a record (no --expires, no agent:message) is re-minted once;
// there is no id to revoke, and that is not an error.
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
	if err := st.SaveControllerPATRecord(state.PATRecord{Requested: controllerPATScopes(false, false), ExpiresAt: now.Add(5 * 24 * time.Hour)}); err != nil {
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

// Revoking the superseded token is best-effort but reported: the new token is
// already persisted and working.
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

// A scion without roles has no ceiling setting, so the step skips it rather
// than fail apply. A probe that cannot answer fails closed, as the --role
// stamp does in scion.Client.Start.
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

// A first mint grants the controller role in the same window, so the issuer
// holds agent.attach on the project before hub token create (scion with
// agent.lifecycle refuses the mint otherwise).
func TestEnsurePATsGrantsControllerRoleBeforeMint(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-controller", "pat-controller-new")
	hub := newFakeAdminHub()
	hub.me = "dev-user"
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	if ctok, _ := st.LoadControllerPAT(); ctok != "pat-controller-new" {
		t.Fatalf("controller PAT = %q", ctok)
	}
	if len(hub.bindings) != 1 || hub.bindings[0].PrincipalID != "dev-user" {
		t.Fatalf("want the issuer bound to %s, got %+v", controllerRoleName, hub.bindings)
	}
}

// A scion upgrade adding agent.lifecycle re-grants the remote role in the next
// window: the jail's scion is asked, not the hub, which may be down. Without
// the probe the recorded grant stands.
func TestEnsurePATsRegrantsRemoteRoleAfterScionUpgrade(t *testing.T) {
	for _, probe := range []bool{false, true} {
		st := state.ForConfig(t.TempDir())
		seedPAT(t, st, "controller", "pat-c")
		// The controller token already carries what the probe asks for; only the
		// remote role is stale.
		if err := st.SaveControllerPATRecord(state.PATRecord{ID: "old-pat-c", Requested: controllerPATScopes(probe, false),
			MintedAt: time.Now(), ExpiresAt: time.Now().Add(300 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		seedPAT(t, st, "remote", "pat-r")
		old := remoteRolePermissions(false)
		if err := st.SaveRemoteRoleRecord(state.RemoteRoleRecord{Permissions: old,
			Bound: map[string]string{"op@github": "user-1"}, Ceilings: map[string]string{"op@github": "c1"},
			CeilingPermissions: old}); err != nil {
			t.Fatal(err)
		}
		f := proc.NewFakeRunner()
		scriptPATMintChain(f)
		hub := newFakeAdminHub("op@github")
		o := patMintOpts{AdminHub: hub}
		if probe {
			o.ScopeKnown = func(_ context.Context, scope string) (bool, error) { return scope == lifecycleScope, nil }
		}
		if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever",
			remoteAccess{Enabled: true, Emails: []string{"op@github"}}, o); err != nil {
			t.Fatal(err)
		}
		opened := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion server start") }) > 0
		if opened != probe {
			t.Fatalf("probe=%v: window opened=%v", probe, opened)
		}
		rec, _, _ := st.LoadRemoteRoleRecord()
		if got := slices.Contains(rec.Permissions, lifecyclePermission); got != probe {
			t.Fatalf("probe=%v: recorded role permissions %v", probe, rec.Permissions)
		}
	}
}

// A window that re-mints only the remote PAT still grants the controller
// role first: the remote PAT asks for agent:attach too.
func TestEnsurePATsGrantsControllerRoleForRemoteOnlyMint(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-c")
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-remote", "pat-remote-new")
	hub := newFakeAdminHub()
	hub.me = "dev-user"
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{Enabled: true}, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	if rtok, _ := st.LoadRemotePAT(); rtok != "pat-remote-new" {
		t.Fatalf("remote PAT = %q", rtok)
	}
	if len(hub.bindings) == 0 || !slices.ContainsFunc(hub.bindings, func(b hubapi.RoleBinding) bool { return b.PrincipalID == "dev-user" }) {
		t.Fatalf("want the issuer bound to %s, got %+v", controllerRoleName, hub.bindings)
	}
}

// On a scion with agent.lifecycle a controller token without agent:lifecycle
// cannot resume the manager (409 "already exists"). The next apply re-mints
// it, after granting the issuer agent.lifecycle through the controller role.
func TestEnsurePATsRemintsControllerWithLifecycle(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-c")
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-controller", "pat-controller-new")
	hub := newFakeAdminHub()
	hub.me = "dev-user"
	o := patMintOpts{AdminHub: hub, ScopeKnown: func(_ context.Context, scope string) (bool, error) { return scope == lifecycleScope, nil }}
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, o); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := st.LoadControllerPATRecord()
	if !slices.Contains(rec.Requested, lifecycleScope) {
		t.Fatalf("re-minted controller scopes %v, want %s", rec.Requested, lifecycleScope)
	}
	var role *hubapi.RoleDefinition
	for i := range hub.roles {
		if hub.roles[i].Name == controllerRoleName {
			role = &hub.roles[i]
		}
	}
	if role == nil || !slices.Contains(role.Permissions, lifecyclePermission) {
		t.Fatalf("controller role %+v, want it to hold %s", role, lifecyclePermission)
	}
}

// A scion without agent:lifecycle keeps the old scope set: no re-mint.
func TestEnsurePATsKeepsControllerWithoutLifecycle(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-c")
	f := proc.NewFakeRunner()
	o := patMintOpts{ScopeKnown: func(context.Context, string) (bool, error) { return false, nil }}
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, o); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion server start") }); n != 0 {
		t.Fatalf("the window opened %d time(s) for a token that needs nothing", n)
	}
}

// With the probe unanswered, the token on disk decides: one minted with
// agent:lifecycle is not re-minted without it, nor the reverse.
func TestControllerLifecycleFallsBackToTheRecord(t *testing.T) {
	failing := func(context.Context, string) (bool, error) { return false, errors.New("no scion") }
	with := state.PATRecord{Requested: controllerPATScopes(true, false)}
	without := state.PATRecord{Requested: controllerPATScopes(false, false)}
	if !controllerLifecycle(context.Background(), failing, with) || controllerLifecycle(context.Background(), failing, without) {
		t.Fatal("controllerLifecycle ignores the record when the probe fails")
	}
	if !controllerLifecycle(context.Background(), nil, with) {
		t.Fatal("controllerLifecycle ignores the record with no probe")
	}
}

// A token whose granted set already holds agent:lifecycle (agent:manage
// expands to it) is not re-minted.
func TestControllerLifecycleHonorsTheGrantedSet(t *testing.T) {
	known := func(context.Context, string) (bool, error) { return true, nil }
	rec := state.PATRecord{Requested: controllerPATScopes(false, true), Granted: []string{"agent:attach", lifecycleScope, inboxReadScope}}
	if controllerLifecycle(context.Background(), known, rec) {
		t.Fatal("controllerLifecycle asks for agent:lifecycle although the token was granted it")
	}
	want := controllerPATScopes(controllerLifecycle(context.Background(), known, rec), controllerInbox(context.Background(), known, rec))
	if patMintReason("tok", rec, true, want, time.Now()) != "" {
		t.Fatal("a token granted agent:lifecycle is re-minted")
	}
}

// inbox:read follows the jail's scion like agent:lifecycle: asked for when
// scion knows it, never when it does not, and the record decides when the
// probe cannot answer.
func TestControllerInbox(t *testing.T) {
	failing := func(context.Context, string) (bool, error) { return false, errors.New("no scion") }
	knows := func(_ context.Context, scope string) (bool, error) { return scope == inboxReadScope, nil }
	knowsNothing := func(context.Context, string) (bool, error) { return false, nil }
	with := state.PATRecord{Requested: controllerPATScopes(false, true)}
	without := state.PATRecord{Requested: controllerPATScopes(false, false)}
	for _, tc := range []struct {
		name  string
		known scopeKnownFunc
		rec   state.PATRecord
		want  bool
	}{
		{"scion knows it", knows, without, true},
		{"scion does not know it", knowsNothing, with, false},
		{"probe fails, record has it", failing, with, true},
		{"probe fails, record lacks it", failing, without, false},
		{"no probe, record has it", nil, with, true},
		{"no probe, no record", nil, state.PATRecord{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controllerInbox(context.Background(), tc.known, tc.rec); got != tc.want {
				t.Fatalf("controllerInbox = %v, want %v", got, tc.want)
			}
		})
	}
}

// The scope set: the baseline-ceiling reads always, lifecycle and inbox only
// when asked for.
func TestControllerPATScopes(t *testing.T) {
	reads := []string{"skill:read", "skill:list", "template:read", "template:list", "harness_config:read", "harness_config:list"}
	for _, tc := range []struct {
		lifecycle, inbox bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		got := controllerPATScopes(tc.lifecycle, tc.inbox)
		for _, s := range append([]string{"agent:manage", "agent:attach", "agent:message", "project:read", "project:update"}, reads...) {
			if !slices.Contains(got, s) {
				t.Errorf("(%v, %v): %v lacks %s", tc.lifecycle, tc.inbox, got, s)
			}
		}
		if slices.Contains(got, lifecycleScope) != tc.lifecycle || slices.Contains(got, inboxReadScope) != tc.inbox {
			t.Errorf("(%v, %v): got %v", tc.lifecycle, tc.inbox, got)
		}
	}
}

// A pin bump that teaches the jail's scion inbox:read re-mints a controller
// token minted without it, says why, and mints with the scope.
func TestEnsureControllerPATReMintsForInboxRead(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-old")
	f := patMintRunner("scion_pat_new")
	var warned []string
	o := patMintOpts{
		ScopeKnown: func(_ context.Context, scope string) (bool, error) { return scope == inboxReadScope, nil },
		Warn:       func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) },
	}
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, o); err != nil {
		t.Fatal(err)
	}
	i := f.CallIndex(proc.ArgvPrefix("scion", "hub", "token", "create"))
	if i < 0 {
		t.Fatal("no token was minted")
	}
	if argv := f.Calls[i].Argv(); !strings.Contains(argv, inboxReadScope) {
		t.Fatalf("mint argv %q lacks %s", argv, inboxReadScope)
	}
	if !slices.ContainsFunc(warned, func(w string) bool {
		return strings.Contains(w, "re-minting lever-controller") && strings.Contains(w, inboxReadScope)
	}) {
		t.Fatalf("no re-mint warning naming %s: %q", inboxReadScope, warned)
	}
	rec, _, _ := st.LoadControllerPATRecord()
	if !slices.Contains(rec.Requested, inboxReadScope) {
		t.Fatalf("record requested %v lacks %s", rec.Requested, inboxReadScope)
	}
}
