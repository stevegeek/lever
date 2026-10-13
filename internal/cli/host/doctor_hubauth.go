package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
)

// doctorHubAuth is what the hub checks need to speak as lever's identities.
// Doctor never creates the key: a missing one is a finding.
type doctorHubAuth struct {
	jr     proc.Runner
	st     state.State
	key    *hubauth.Key
	id     string
	clock  hubauth.Clock
	signer *hubauth.Signer
	err    error
}

func newDoctorHubAuth(ctx context.Context, jr proc.Runner, st state.State) doctorHubAuth {
	d := doctorHubAuth{jr: jr, st: st}
	if jr == nil {
		d.err = errors.New("no jail transport")
		return d
	}
	if d.key, d.err = hubauth.LoadKey(st.HubAuthKey()); d.err != nil {
		return d
	}
	if d.id, d.err = st.ReadInstanceID(); d.err != nil {
		return d
	}
	d.clock = readGuestClock(ctx, jr, func(string, ...any) {})
	d.signer = hubauth.NewSigner(d.key, d.id, d.clock)
	return d
}

func (d doctorHubAuth) client(sign func() (string, error)) *hubapi.Client {
	return &hubapi.Client{T: &hubapi.AssertCurl{Runner: d.jr, BaseURL: scionpkg.DefaultHubEndpoint, Sign: sign}}
}

const hubAuthFix = "run `lever apply`; it installs the key, pins the hub's auth settings and restarts the hub when they differ"

// hubAuthState is the jail-side evidence the authentication row reads besides
// the hub's answers: the public key file and scion's dev token.
const hubAuthStateScript = `[ -e "$HOME/$1" ] && echo devtoken
[ -f "$2" ] && cat "$2"`

// checkHubAuthentication tests the hub's effective configuration by
// behaviour, which file contents cannot prove: an assertion from another key
// and one for another hub are refused, lever-admin is the admin, no OAuth
// login is offered, the running hub was started with lever's auth env and
// without dev auth, and no dev token lies on disk.
func checkHubAuthentication(ctx context.Context, d doctorHubAuth, running func(context.Context) (scionpkg.RunningHub, error)) checkResult {
	const name = "hub authentication"
	if d.err != nil {
		return checkResult{name, false, "cannot sign hub assertions: " + d.err.Error(), hubAuthFix}
	}
	var problems []string
	other, err := hubauth.NewEphemeralKey()
	if err != nil {
		return checkResult{name, false, err.Error(), ""}
	}
	forged := hubauth.NewSigner(other, d.id, d.clock)
	foreign := hubauth.NewSigner(d.key, d.id+"-doctor-elsewhere", d.clock)
	for _, c := range []struct {
		what string
		sign func() (string, error)
	}{{"an assertion signed by another key", forged.Admin}, {"an assertion for another hub (iss/aud)", foreign.Admin}} {
		status, _, err := d.client(c.sign).T.Do(ctx, http.MethodGet, "/api/v1/auth/me")
		switch {
		case err != nil:
			return checkResult{name, false, "could not reach the hub from the jail: " + err.Error(), "is the machine up? `lever apply`"}
		case status != http.StatusUnauthorized:
			problems = append(problems, fmt.Sprintf("the hub answered %d to %s, want 401", status, c.what))
		}
	}
	me, err := d.client(d.signer.Admin).Me(ctx)
	if err != nil {
		problems = append(problems, "lever-admin's assertion was refused: "+err.Error())
	} else if u, ok, err := d.client(d.signer.Admin).GetUser(ctx, me.ID); err != nil || !ok || u.Role != "admin" {
		problems = append(problems, fmt.Sprintf("lever-admin (%s) is not a hub admin", me.ID))
	}
	for _, ct := range []string{"web", "cli"} {
		res, err := d.jr.Run(ctx, nil, "curl", "-q", "-sS", "--noproxy", "*", "-m", "10",
			scionpkg.DefaultHubEndpoint+"/api/v1/auth/providers?clientType="+ct)
		if err != nil {
			problems = append(problems, "could not read the hub's login providers: "+firstLine(err.Error()))
			break
		}
		if !strings.Contains(res.Stdout, `"providers":[]`) && !strings.Contains(res.Stdout, `"providers":null`) {
			problems = append(problems, "the hub offers a "+ct+" login provider (an oidc_login or OAuth block is configured)")
		}
	}
	if running != nil {
		if live, err := running(ctx); err != nil {
			problems = append(problems, "could not read the running hub: "+err.Error())
		} else if live.Running {
			want := apply.HubAuthEnv(d.id, d.key.Fingerprint())
			if why := apply.HubRestartReason(live, scionpkg.ServerOpts{Env: want, WebAssetsDir: flagValueOf(live.Argv, "--web-assets-dir")}); why != "" {
				problems = append(problems, "the running hub does not carry lever's auth settings: "+why)
			}
		}
	}
	res, err := d.jr.Run(ctx, nil, "sh", "-c", hubAuthStateScript, "_", layout.DevTokenRel, hubauth.GuestPublicKeyPath)
	if err != nil {
		problems = append(problems, "could not read the jail: "+firstLine(err.Error()))
	} else {
		out := res.Stdout
		if strings.HasPrefix(out, "devtoken\n") {
			problems = append(problems, "scion's dev token ~/"+layout.DevTokenRel+" exists in the jail: a hub admin credential")
			out = strings.TrimPrefix(out, "devtoken\n")
		}
		if !bytes.Equal(bytes.TrimSpace([]byte(out)), bytes.TrimSpace(d.key.PublicPEM())) {
			problems = append(problems, hubauth.GuestPublicKeyPath+" in the jail is not the public half of "+stateRel(d.st, d.st.HubAuthKey()))
		}
	}
	if len(problems) > 0 {
		return checkResult{name, false, strings.Join(problems, "; "), hubAuthFix}
	}
	return checkResult{name, true, "forged and foreign assertions refused, lever-admin is admin, no login provider, hub runs lever's auth env, no dev token, key " + d.key.Fingerprint(), ""}
}

func flagValueOf(argv []string, name string) string {
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
		if a == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// checkHubIdentities checks the identities hub-identity converges: lever-admin
// is the only admin by role and by binding, the controller holds only the
// tokens lever recorded, and scion's dev user is where the mode says.
func checkHubIdentities(ctx context.Context, d doctorHubAuth) checkResult {
	const name = "hub identities"
	if d.err != nil {
		return checkResult{name, false, "cannot sign hub assertions: " + d.err.Error(), hubAuthFix}
	}
	rec, found, err := d.st.LoadHubIdentity()
	switch {
	case err != nil:
		return checkResult{name, false, err.Error(), "run `lever apply`"}
	case !found:
		return checkResult{name, false, "no hub identity record (" + stateRel(d.st, d.st.HubIdentityFile()) + ")", "run `lever apply`"}
	}
	admin := d.client(d.signer.Admin)
	var problems []string
	if rec.KeyFingerprint != d.key.Fingerprint() {
		problems = append(problems, fmt.Sprintf("the record names key %s, the host holds %s", rec.KeyFingerprint, d.key.Fingerprint()))
	}
	if admins, err := admin.ListAdmins(ctx); err != nil {
		problems = append(problems, "could not list admins: "+err.Error())
	} else {
		for _, a := range admins {
			if a.UserID != rec.AdminID {
				problems = append(problems, fmt.Sprintf("%s (%s) is a hub admin (role %v, super-admin bindings %d); only lever-admin may be", a.UserID, a.Email, a.ByRole, len(a.BindingIDs)))
			}
		}
	}
	want := map[string]bool{}
	for _, load := range []func() (state.PATRecord, bool, error){d.st.LoadControllerPATRecord, d.st.LoadRemotePATRecord} {
		if r, ok, _ := load(); ok && r.ID != "" {
			want[r.ID] = true
		}
	}
	if toks, err := d.client(d.signer.Controller).ListTokens(ctx); err != nil {
		problems = append(problems, "could not list the controller's tokens: "+err.Error())
	} else {
		var extra []string
		for _, t := range toks {
			if !t.Revoked && !want[t.ID] {
				extra = append(extra, t.ID+" ("+t.Name+")")
			}
		}
		if len(extra) > 0 {
			sort.Strings(extra)
			problems = append(problems, "the controller holds tokens lever did not record: "+strings.Join(extra, ", "))
		}
	}
	dev, devFound, err := admin.GetUser(ctx, devUserID)
	switch {
	case err != nil:
		problems = append(problems, "could not read scion's dev user: "+err.Error())
	case rec.Mode == state.HubModeMigrated && (!devFound || !strings.EqualFold(dev.Email, hubauth.ControllerEmail) || dev.Role != "member"):
		problems = append(problems, fmt.Sprintf("the controller (scion's dev user) is %q role %q; want %s, member (it was renamed since the last apply?)", dev.Email, dev.Role, hubauth.ControllerEmail))
	case rec.Mode == state.HubModeFresh && devFound:
		problems = append(problems, "scion's dev user exists on a hub lever brought up without dev auth: something started this hub with dev auth")
	}
	if len(problems) > 0 {
		return checkResult{name, false, strings.Join(problems, "; "), "run `lever apply` (it repairs renames and demotes extra admins); a token lever did not record can be revoked with scion"}
	}
	return checkResult{name, true, fmt.Sprintf("%s mode; lever-admin %s is the only admin; controller %s holds only lever's tokens", rec.Mode, rec.AdminID, rec.ControllerID), ""}
}

// checkRemoteWebRole reads the hub, not lever's record: every configured
// login's hub user is the recorded one, bound to its tier's role and capped,
// and every removed login is suspended.
func checkRemoteWebRole(ctx context.Context, app *config.App, d doctorHubAuth) checkResult {
	const name = "remote web role"
	users := remoteUsersFor(app)
	rec, _, err := d.st.LoadHubIdentity()
	if err != nil {
		return checkResult{name, false, err.Error(), "run `lever apply`"}
	}
	if len(users) == 0 && len(rec.Users) == 0 {
		return checkResult{name, true, "no remote logins", ""}
	}
	if d.err != nil {
		return checkResult{name, false, "cannot sign hub assertions: " + d.err.Error(), hubAuthFix}
	}
	admin := d.client(d.signer.Admin)
	defs, err := admin.RoleDefinitions(ctx)
	if err != nil {
		return checkResult{name, false, "could not list roles: " + err.Error(), "is the machine up? `lever apply`"}
	}
	roleID := map[bool]string{}
	if r, ok := projectRoleNamed(defs, remoteWebRoleName); ok {
		roleID[false] = r.ID
	}
	if r, ok := projectRoleNamed(defs, contactRoleName); ok {
		roleID[true] = r.ID
	}
	var problems, ok []string
	configured := map[string]bool{}
	for _, u := range users {
		configured[u.Email] = true
		hu, found, err := admin.UserByEmail(ctx, u.Email)
		switch {
		case err != nil:
			problems = append(problems, u.Email+": "+err.Error())
			continue
		case !found:
			problems = append(problems, u.Email+" has no hub user (not invited yet)")
			continue
		case rec.Users[u.Email] != hu.ID:
			problems = append(problems, fmt.Sprintf("%s is hub user %s, lever recorded %q (the proxy refuses this login)", u.Email, hu.ID, rec.Users[u.Email]))
			continue
		case hu.Status == "suspended":
			problems = append(problems, u.Email+" is suspended")
			continue
		}
		bs, err := admin.UserRoleBindings(ctx, hu.ID)
		if err != nil {
			problems = append(problems, u.Email+": "+err.Error())
			continue
		}
		bound := slices.ContainsFunc(bs, func(b hubapi.RoleBinding) bool {
			return b.RoleDefinitionID == roleID[u.Contact] && roleID[u.Contact] != "" && b.ScopeID == rec.ProjectID
		})
		wrong := slices.ContainsFunc(bs, func(b hubapi.RoleBinding) bool {
			return b.RoleDefinitionID == roleID[!u.Contact] && roleID[!u.Contact] != "" && b.ScopeID == rec.ProjectID
		})
		// A ceiling counts only when its maximum keeps the tier's limit: exactly
		// the contact role's permissions, or no project.create for an operator.
		capped := false
		for _, n := range remoteCeilingNames(u.Email) {
			cs, err := admin.AccessConstraintsNamed(ctx, n)
			if err != nil {
				continue
			}
			for _, c := range cs {
				if c.Name != n || c.Subject.PrincipalID != hu.ID || c.ScopeType != hubapi.ConstraintScopeSystem {
					continue
				}
				d, err := admin.AccessConstraint(ctx, c.ID)
				if err != nil {
					continue
				}
				if u.Contact && hubapi.SamePermissions(d.MaximumPermissions, contactRolePermissions()) ||
					!u.Contact && len(d.MaximumPermissions) > 0 && !slices.Contains(d.MaximumPermissions, projectCreatePermission) {
					capped = true
				}
			}
		}
		tier := map[bool]string{false: remoteWebRoleName, true: contactRoleName}[u.Contact]
		switch {
		case !bound:
			problems = append(problems, u.Email+" is not bound to "+tier)
		case wrong:
			problems = append(problems, u.Email+" is also bound to the other tier's role")
		case !capped:
			problems = append(problems, u.Email+" has no ceiling that keeps its tier's limit")
		default:
			ok = append(ok, u.Email+" ("+tier+", "+hu.Status+")")
		}
	}
	for email, id := range rec.Users {
		if configured[email] {
			continue
		}
		if hu, found, err := admin.GetUser(ctx, id); err == nil && found && hu.Status != "suspended" {
			problems = append(problems, email+" was removed from the config but its hub user is "+hu.Status)
		}
	}
	if len(problems) > 0 {
		return checkResult{name, false, strings.Join(problems, "; "), "run `lever apply` (or `lever reload`): it invites, caps and binds every login, and suspends removed ones, while agents run"}
	}
	return checkResult{name, true, "bound and capped before sign-in: " + strings.Join(ok, ", "), ""}
}
