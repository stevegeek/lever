package host

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/state"
)

// seedPreRecordTokens adds tokens an older lever minted as the migrated
// controller before it kept records: two PATs, an interrupted `lever ssh`
// one long expired, and one still in its life (a session starting now).
// Another user's token is never the controller's to list.
func seedPreRecordTokens(e *hiEnv) {
	h := e.hub
	other := h.addUser("", "eve@example.com", "member", "active", e.now)
	h.tokens = append(h.tokens,
		&hiToken{ID: "tok-pre-ctl", Owner: devUserID, Name: "lever-controller", ProjectID: "proj-1", Secret: "scion_pat_prectl000000x", Expires: e.now.Add(200 * 24 * time.Hour)},
		&hiToken{ID: "tok-pre-remote", Owner: devUserID, Name: "lever-remote", ProjectID: "proj-1", Secret: "scion_pat_prerem000000x", Expires: e.now.Add(200 * 24 * time.Hour)},
		&hiToken{ID: "tok-ssh-old", Owner: devUserID, Name: sshTokenName, ProjectID: "proj-1", Secret: "scion_pat_sshold000000x", Expires: e.now.Add(-24 * time.Hour)},
		&hiToken{ID: "tok-ssh-new", Owner: devUserID, Name: sshTokenName, ProjectID: "proj-1", Secret: "scion_pat_sshnew000000x", Expires: time.Now().Add(100 * 365 * 24 * time.Hour)},
		&hiToken{ID: "tok-eve", Owner: other.ID, Name: "mine", Secret: "scion_pat_eve000000000x", Expires: e.now.Add(24 * time.Hour)},
	)
}

func hiRevoked(h *hiHub) []string {
	var out []string
	for _, ev := range h.events {
		if id, ok := strings.CutPrefix(ev, "revoke "); ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// TestHubIdentityRevokesUnrecordedTokens: an instance migrated from a lever
// that minted PATs before it kept records ends, after one apply, with the
// controller holding only lever's tokens, and doctor's row green. The current
// PAT, a `lever ssh` token that may be starting, and other users' tokens are
// never revoked; a second apply revokes nothing.
func TestHubIdentityRevokesUnrecordedTokens(t *testing.T) {
	ctx := context.Background()
	d := newHIDoctor(t, hiRemoteApp(false))
	d.seedMigrated("scion@localhost", false)
	seedPreRecordTokens(d.hiEnv)

	d.mustRun()
	if got := strings.Join(hiRevoked(d.hub), ","); got != "tok-pre-ctl,tok-pre-remote,tok-ssh-old" {
		t.Fatalf("revoked %s", got)
	}
	if !d.diskPATIs(d.st.LoadControllerPAT, "tok-old") {
		t.Fatal("the controller PAT lever uses changed")
	}
	if !hiAnyContains(d.logs, "revoked hub token tok-pre-ctl (lever-controller)") {
		t.Fatalf("logs %q", d.logs)
	}
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), true, "migrated mode")

	n := len(d.hub.events)
	d.mustRun()
	if got := hiEventsWith(d.hub.eventsFrom(n), "revoke"); len(got) > 0 {
		t.Fatalf("second apply revoked %q", got)
	}
}

// TestHubIdentityRevokesTokensLeftByAnEarlierMigration: an instance an
// earlier lever already migrated, whose doctor row stays red with the
// tokens it never revoked, is green after one apply.
func TestHubIdentityRevokesTokensLeftByAnEarlierMigration(t *testing.T) {
	ctx := context.Background()
	d := newHIDoctor(t, hiRemoteApp(false))
	d.seedMigrated("scion@localhost", false)
	d.mustRun()
	seedPreRecordTokens(d.hiEnv)
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), false, "tok-pre-ctl (lever-controller)", "tok-pre-remote (lever-remote)", "tok-ssh-old")
	d.mustRun()
	if got := strings.Join(hiRevoked(d.hub), ","); got != "tok-pre-ctl,tok-pre-remote,tok-ssh-old" {
		t.Fatalf("revoked %s", got)
	}
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), true, "migrated mode")
}

// TestHubIdentityKeepsAHeldUnrecordedToken: a token whose secret lever holds
// is never revoked, recorded or not; one the hub lists with no prefix is only
// reported, by apply and by doctor.
func TestHubIdentityKeepsAHeldUnrecordedToken(t *testing.T) {
	ctx := context.Background()
	d := newHIDoctor(t, hiRemoteApp(true))
	d.mustRun()
	ctl := d.hub.byEmail(hubauth.ControllerEmail)
	if ctl == nil {
		t.Fatal("no controller row")
	}
	// The remote proxy's token on disk, its record lost.
	if err := os.Remove(d.st.RemotePATRecord()); err != nil {
		t.Fatal(err)
	}
	remote := hiTokenNamed(d.hub, "lever-remote", ctl.ID)
	d.hub.tokens = append(d.hub.tokens, &hiToken{ID: "tok-noprefix", Owner: ctl.ID, Name: "lever-controller", Expires: d.now.Add(time.Hour)})
	n := len(d.hub.events)
	d.step().revokeUnrecordedTokens(ctx, state.HubModeFresh, ctl.ID)
	if got := hiEventsWith(d.hub.eventsFrom(n), "revoke"); len(got) > 0 {
		t.Fatalf("revoked %q", got)
	}
	if remote.Revoked {
		t.Fatal("the remote PAT lever holds was revoked")
	}
	if !hiAnyContains(d.warns, "tok-noprefix (lever-controller)", "is not revoked") {
		t.Fatalf("warns %q", d.warns)
	}
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), false, "cannot tell from its own", "tok-noprefix")
}

// TestRevokeUnrecordedTokensFailsClosed: anything that leaves the list in
// doubt revokes nothing and warns.
func TestRevokeUnrecordedTokensFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, warn string
		setup      func(e *hiEnv) (controllerID string)
	}{
		{"unreadable record", "cannot read its own token records", func(e *hiEnv) string {
			if err := os.WriteFile(e.st.ControllerPATRecord(), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			return devUserID
		}},
		{"no controller PAT", "holds no controller token", func(e *hiEnv) string {
			if err := os.Remove(e.st.ControllerPAT()); err != nil {
				t.Fatal(err)
			}
			return devUserID
		}},
		{"another controller", "not the recorded controller", func(*hiEnv) string { return "user-else" }},
		{"list fails", "listing them failed", func(e *hiEnv) string {
			e.hub.down = map[string]int{"GET /api/v1/auth/tokens": 500}
			return devUserID
		}},
		{"PAT not listed", "does not show the controller token", func(e *hiEnv) string {
			if err := e.st.SaveControllerPAT("scion_pat_notlisted0000x"); err != nil {
				t.Fatal(err)
			}
			return devUserID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHIEnv(t, hiRemoteApp(false))
			e.seedMigrated("scion@localhost", false)
			e.mustRun()
			seedPreRecordTokens(e)
			id := tc.setup(e)
			n := len(e.hub.events)
			e.step().revokeUnrecordedTokens(ctx, state.HubModeMigrated, id)
			if got := hiEventsWith(e.hub.eventsFrom(n), "revoke"); len(got) > 0 {
				t.Fatalf("revoked %q", got)
			}
			if !hiAnyContains(e.warns, tc.warn) {
				t.Fatalf("warns %q, want %q", e.warns, tc.warn)
			}
		})
	}
}

// TestLeverTokensUnrecorded is the sorting on its own: recorded ids, held
// secrets, revoked tokens and a live `lever ssh` token are lever's or
// harmless; a token without a usable prefix is unknown, never revoked.
func TestLeverTokensUnrecorded(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	k := leverTokens{ids: map[string]bool{"rec": true}, controller: "scion_pat_held111122223333", secrets: []string{"scion_pat_held111122223333"}}
	toks := []hubapi.Token{
		{ID: "rec", Name: "lever-controller", Prefix: "scion_pat_aaaa"},
		{ID: "held", Name: "lever-controller", Prefix: "scion_pat_held1111"},
		{ID: "gone", Name: "lever-controller", Prefix: "scion_pat_bbbb", Revoked: true},
		{ID: "ssh-live", Name: sshTokenName, Prefix: "scion_pat_cccc", ExpiresAt: now.Add(time.Hour)},
		{ID: "ssh-dead", Name: sshTokenName, Prefix: "scion_pat_dddd", ExpiresAt: now.Add(-time.Hour)},
		{ID: "bare", Name: "lever-remote", Prefix: "scion_pat_"},
		{ID: "none", Name: "x"},
		{ID: "odd", Name: "x", Prefix: "other_eeee"},
		{ID: "old", Name: "lever-remote", Prefix: "scion_pat_ffff"},
	}
	revoke, unknown := k.unrecorded(toks, now)
	ids := func(ts []hubapi.Token) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(revoke); got != "ssh-dead,old" {
		t.Fatalf("revoke = %s", got)
	}
	if got := ids(unknown); got != "bare,none,odd" {
		t.Fatalf("unknown = %s", got)
	}
	if !k.controllerListed(toks) || k.controllerListed(toks[2:3]) || (leverTokens{}).controllerListed(toks) {
		t.Fatal("controllerListed")
	}
}
