package host

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
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
	rec := state.PATRecord{Requested: controllerPATScopes(false, false), Granted: []string{"agent:attach", lifecycleScope}}
	if controllerLifecycle(context.Background(), known, rec) {
		t.Fatal("controllerLifecycle asks for agent:lifecycle although the token was granted it")
	}
	if patMintReason("tok", rec, true, controllerPATScopes(controllerLifecycle(context.Background(), known, rec), false), time.Now()) != "" {
		t.Fatal("a token granted agent:lifecycle is re-minted")
	}
}

func TestPatUrgent(t *testing.T) {
	now := time.Now()
	want := controllerPATScopes(false, false)
	rec := func(scopes []string, exp time.Time) state.PATRecord {
		return state.PATRecord{Requested: scopes, ExpiresAt: exp}
	}
	for _, tc := range []struct {
		name  string
		tok   string
		rec   state.PATRecord
		found bool
		want  bool
	}{
		{"current", "t", rec(want, now.Add(300*24*time.Hour)), true, false},
		{"renew window", "t", rec(want, now.Add(5*24*time.Hour)), true, false},
		{"absent", "", state.PATRecord{}, false, true},
		{"no record", "t", state.PATRecord{}, false, true},
		{"scope drift", "t", rec([]string{"agent:manage"}, now.Add(300*24*time.Hour)), true, true},
		{"expired", "t", rec(want, now.Add(-time.Hour)), true, true},
	} {
		if got := patUrgent(tc.tok, tc.rec, tc.found, want, now); got != tc.want {
			t.Errorf("%s: patUrgent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestApplySignalContextCancelsOnSIGINT(t *testing.T) {
	ctx, stop := applySignalContext(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the apply context")
	}
}

// TestRemotePATScopesAreReadOnly: the proxy only lists agents with the remote
// PAT, and it is the controller's token, so it must not attach, message or
// change anything.
func TestRemotePATScopesAreReadOnly(t *testing.T) {
	got := remotePATScopes()
	if !slices.Equal(got, []string{"agent:read", "agent:list", "project:read"}) {
		t.Fatalf("remotePATScopes() = %v", got)
	}
	perms := remoteRolePermissions(false)
	for _, want := range []string{"agent.attach", "agent.message", "project.list"} {
		if !slices.Contains(perms, want) {
			t.Fatalf("the web role lost %s: %v", want, perms)
		}
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
