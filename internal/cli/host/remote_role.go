package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/state"
)

// remoteWebRoleName is the custom project role lever binds the remote web
// UI's hub users to.
//
// Why it exists: `lever remote` signs the browser in through lever's local
// OIDC provider, and the proxy sends that session on EVERY hub request, API
// included, so the whole web UI runs as a HUB USER (one per allowed login,
// see remoteproxy.HubUserEmails). This role, plus the ceiling below, is where
// the remote surface is narrowed now. Since scion's permission rework a signed-in user holds only the system hub-members
// binding and no role on the instance project, so the chat and the /events
// stream answer 403. No lever token can fix that — scion denies UATs on
// hub-level resources, and role and binding admin is hub-admin only — so the
// grant runs in the bootstrap-token step's throwaway dev-auth window, where
// the dev identity is the super-admin.
const remoteWebRoleName = "lever-remote"

const remoteWebRoleDescription = "Managed by lever: the remote web UI's read, attach and message surface on this project."

// remoteRolePermissions is the permission set of the remote web role: the
// SAME surface the remote PAT is minted with (the PAT the proxy used to
// inject), derived from remotePATScopes so the two cannot drift. scion's registry names each permission as its UAT scope with
// a dot for the colon (pkg/hub/permissions/registry.go, UATScope), so the
// mapping is mechanical.
func remoteRolePermissions() []string {
	scopes := remotePATScopes()
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = strings.Replace(s, ":", ".", 1)
	}
	// project.list is web-only and read-only: without it the SPA's project
	// list (GET /api/v1/projects, ResolveListScopes "project.list") comes back
	// empty. The remote PAT does not need it, so it is not a PAT scope.
	return append(out, "project.list")
}

// projectCreatePermission is the one hub-member permission the remote web
// users must not hold: a project a web user created would be owned by that
// user, outside every control lever keeps on the instance project.
const projectCreatePermission = "project.create"

// hubMemberRoleName is scion's system role every hub user holds through the
// hub-members group, which scion backfills on every boot
// (store.SystemRoleHubMember, pkg/hub/seed.go hubMemberPermissionIDs). It
// carries project.create, and lever cannot unbind it: the next boot would
// restore the membership. Hence the ceiling below instead.
const hubMemberRoleName = "hub-member"

// remoteCeilingPurpose is the purpose scion requires on every access
// constraint.
const remoteCeilingPurpose = "Managed by lever: the remote web UI user may not create projects. " +
	"Maximum = hub-member minus project.create, plus the lever-remote project role."

// remoteCeilingName names the access constraint lever keeps on one remote web
// user. scion keeps constraint names unique per scope, and the email is the
// key lever's own record uses.
func remoteCeilingName(email string) string { return "lever-remote-ceiling:" + email }

// remoteCeilingPermissions is the maximum-permission set of each ceiling: the
// hub-member role's permissions as the hub holds them now, minus
// project.create, plus the lever-remote role's permissions. A constraint only
// reduces, and constraints intersect with nothing else lever writes, so this
// set keeps every permission a remote web user holds today except
// project.create — the role's own set must be in it, or the ceiling would cut
// the very chat, attach and read surface the role grants.
//
// It is read from the hub rather than copied from scion's source so a scion
// upgrade that adds a hub-member permission is picked up the next time the
// window runs. Nothing opens the window for that on its own (the record-only
// readers cannot see the hub's roles), so until then the ceiling withholds
// the new permission; deleting remote-role.json forces a recompute.
func remoteCeilingPermissions(hubMember, role []string) []string {
	var out []string
	for _, p := range append(slices.Clone(hubMember), role...) {
		if p != projectCreatePermission && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// remoteAccess is what the bootstrap-token step needs to know about the
// remote block: whether it is on, and the hub user emails to grant.
type remoteAccess struct {
	Enabled bool
	Emails  []string
}

func remoteAccessFor(app *config.App) remoteAccess {
	if !app.RemoteEnabled() {
		return remoteAccess{}
	}
	return remoteAccess{Enabled: true, Emails: remoteproxy.HubUserEmails(app.Remote.AllowedUsers)}
}

// remoteRoleFix is the one repair for a pending user: the hub user exists
// only after its first sign-in, and only a dev-auth window can bind it.
const remoteRoleFix = "sign in once from the phone, then run `lever apply`"

// remoteRoleReason says why the remote web role grant must (re)run, or ""
// when the record covers every email with the current permission set and a
// project-create ceiling. It reads the record only, so `lever doctor` can use
// it without admin creds.
func remoteRoleReason(rec state.RemoteRoleRecord, found bool, emails, perms []string) string {
	if !found {
		return "no grant recorded"
	}
	if !hubapi.SamePermissions(rec.Permissions, perms) {
		return fmt.Sprintf("the role was granted with %s; this lever needs %s",
			strings.Join(rec.Permissions, ","), strings.Join(perms, ","))
	}
	if len(rec.Ceilings) > 0 && !ceilingPermissionsFit(rec.CeilingPermissions, perms) {
		return fmt.Sprintf("the project-create ceiling was written with %s; it must hold %s and not %s",
			strings.Join(rec.CeilingPermissions, ","), strings.Join(perms, ","), projectCreatePermission)
	}
	var pending, unbound, uncapped []string
	for _, e := range emails {
		if _, ok := rec.Bound[e]; ok {
			if rec.Ceilings[e] == "" {
				uncapped = append(uncapped, e)
			}
			continue
		}
		if slices.Contains(rec.Pending, e) {
			pending = append(pending, e)
		} else {
			unbound = append(unbound, e)
		}
	}
	switch {
	case len(unbound) > 0:
		return "not bound yet: " + strings.Join(unbound, ", ")
	case len(uncapped) > 0:
		return remoteCeilingMissing + ": " + strings.Join(uncapped, ", ")
	case len(pending) > 0:
		return "no hub user yet (never signed in): " + strings.Join(pending, ", ")
	}
	return ""
}

// remoteCeilingMissing starts the reason for a bound user with no ceiling
// recorded. Unlike the other reasons the web UI works; it can also create
// projects, so doctor says that instead of "403".
const remoteCeilingMissing = "no project-create ceiling yet, so the web UI can create projects"

// ceilingPermissionsFit reports whether a recorded ceiling keeps the role's
// permissions and withholds project.create. The rest of the set comes from
// the hub's hub-member role, which the record-only readers cannot see.
func ceilingPermissionsFit(ceiling, role []string) bool {
	if slices.Contains(ceiling, projectCreatePermission) {
		return false
	}
	for _, p := range role {
		if !slices.Contains(ceiling, p) {
			return false
		}
	}
	return true
}

// readDevToken reads the throwaway hub's dev token from the jail user's
// ~/.scion/dev-token, the file scion writes when a dev-auth server starts
// (pkg/apiclient/devauth.go) and ensureControllerPAT's deferred cleanup
// deletes. The value goes straight into a JailCurl env; it is never printed.
func readDevToken(ctx context.Context, jr proc.Runner) (string, error) {
	res, err := jr.Run(ctx, nil, "sh", "-c", `cat "$HOME/`+layout.DevTokenRel+`"`)
	if err != nil {
		return "", fmt.Errorf("reading the throwaway hub's dev token: %w", err)
	}
	tok := strings.TrimSpace(res.Stdout)
	if tok == "" {
		return "", errors.New("the throwaway hub's dev token file is empty")
	}
	return tok, nil
}

// ensureRemoteWebRole makes the hub hold the remote web role with exactly
// remoteRolePermissions, and a binding of each email's hub user to it on the
// instance project. It runs as the dev identity (hc is the throwaway hub) and
// returns the record to persist.
//
// Idempotent: a role that exists is reused (and its permission set rewritten
// if it drifted), a binding that exists is not created again. An email with
// no hub user yet is not an error — the user appears on first sign-in — so it
// is listed in Pending and warned about once.
//
// Each bound user also gets a project-create ceiling (ensureRemoteCeiling).
//
// Bindings and ceilings for a user REMOVED from allowed_users are left in
// place: lever does not revoke here (see the remote-access guide).
func ensureRemoteWebRole(ctx context.Context, hc *hubapi.Client, projectKey string, emails []string, now time.Time, warn func(string, ...any)) (state.RemoteRoleRecord, error) {
	perms := remoteRolePermissions()
	projectID, err := hc.ProjectID(ctx, projectKey, fmt.Sprintf("http://127.0.0.1:%d", throwawayHubPort))
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}

	defs, err := hc.RoleDefinitions(ctx)
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}
	role, err := ensureRemoteRoleDefinition(ctx, hc, defs, perms, warn)
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}
	hubMember, ok := systemRole(defs, hubMemberRoleName)
	if !ok {
		return state.RemoteRoleRecord{}, fmt.Errorf("the hub lists no %s system role, so lever cannot compute the project-create ceiling", hubMemberRoleName)
	}
	ceiling := remoteCeilingPermissions(hubMember.Permissions, perms)

	rec := state.RemoteRoleRecord{
		RoleID: role.ID, Permissions: perms, ProjectID: projectID,
		Bound: map[string]string{}, Ceilings: map[string]string{},
		CeilingPermissions: ceiling, GrantedAt: now,
	}
	for _, email := range emails {
		u, found, err := hc.UserByEmail(ctx, email)
		if err != nil {
			return state.RemoteRoleRecord{}, fmt.Errorf("looking up hub user %s: %w", email, err)
		}
		if !found {
			rec.Pending = append(rec.Pending, email)
			continue
		}
		if err := ensureRemoteRoleBinding(ctx, hc, role.ID, u.ID, projectID); err != nil {
			return state.RemoteRoleRecord{}, fmt.Errorf("binding hub user %s to role %s: %w", email, remoteWebRoleName, err)
		}
		rec.Bound[email] = u.ID
		id, err := ensureRemoteCeiling(ctx, hc, email, u.ID, ceiling, warn)
		if err != nil {
			return state.RemoteRoleRecord{}, fmt.Errorf("capping hub user %s (no project create): %w", email, err)
		}
		rec.Ceilings[email] = id
	}
	if len(rec.Pending) > 0 {
		warn("remote web role: no hub user yet for %s, so the web UI will answer 403 for it; %s",
			strings.Join(rec.Pending, ", "), remoteRoleFix)
	}
	return rec, nil
}

// ensureRemoteRoleDefinition finds the lever role (by name, project scope)
// or creates it, and converges its permission set on perms. The hub's answer
// is checked, so a write the hub did not keep fails rather than records.
func ensureRemoteRoleDefinition(ctx context.Context, hc *hubapi.Client, defs []hubapi.RoleDefinition, perms []string, warn func(string, ...any)) (hubapi.RoleDefinition, error) {
	var err error
	var role hubapi.RoleDefinition
	for _, d := range defs {
		if d.Name == remoteWebRoleName && d.ScopeType == hubapi.RoleScopeProject {
			role = d
			break
		}
	}
	want := hubapi.RoleDefinition{
		ID: role.ID, Name: remoteWebRoleName, Description: remoteWebRoleDescription,
		ScopeType: hubapi.RoleScopeProject, Permissions: perms,
	}
	switch {
	case role.ID == "":
		if role, err = hc.CreateRoleDefinition(ctx, want); err != nil {
			return hubapi.RoleDefinition{}, fmt.Errorf("creating role %s: %w", remoteWebRoleName, err)
		}
	case role.System:
		return hubapi.RoleDefinition{}, fmt.Errorf("role %s is a scion system role; lever will not modify it", remoteWebRoleName)
	case !hubapi.SamePermissions(role.Permissions, perms):
		warn("remote web role: role %s has permissions %s; rewriting them to %s",
			remoteWebRoleName, strings.Join(role.Permissions, ","), strings.Join(perms, ","))
		if role, err = hc.UpdateRoleDefinition(ctx, want); err != nil {
			return hubapi.RoleDefinition{}, fmt.Errorf("updating role %s: %w", remoteWebRoleName, err)
		}
	default:
		return role, nil
	}
	if role.ID == "" || !hubapi.SamePermissions(role.Permissions, perms) {
		return hubapi.RoleDefinition{}, fmt.Errorf("role %s after the write: id %q, permissions %s; want %s (the hub did not keep it)",
			remoteWebRoleName, role.ID, strings.Join(role.Permissions, ","), strings.Join(perms, ","))
	}
	return role, nil
}

// ensureRemoteRoleBinding binds userID to roleID on projectID unless the user
// already holds that exact binding directly.
func ensureRemoteRoleBinding(ctx context.Context, hc *hubapi.Client, roleID, userID, projectID string) error {
	have, err := hc.UserRoleBindings(ctx, userID)
	if err != nil {
		return err
	}
	for _, b := range have {
		if b.RoleDefinitionID == roleID && b.ScopeType == hubapi.RoleScopeProject && b.ScopeID == projectID {
			return nil
		}
	}
	_, err = hc.CreateRoleBinding(ctx, hubapi.RoleBinding{
		RoleDefinitionID: roleID, PrincipalType: hubapi.PrincipalUser, PrincipalID: userID,
		ScopeType: hubapi.RoleScopeProject, ScopeID: projectID,
	})
	if errors.Is(err, hubapi.ErrBindingExists) {
		return nil
	}
	return err
}

// systemRole finds a scion system role by name.
func systemRole(defs []hubapi.RoleDefinition, name string) (hubapi.RoleDefinition, bool) {
	for _, d := range defs {
		if d.Name == name && d.System && d.ScopeType == hubapi.RoleScopeSystem {
			return d, true
		}
	}
	return hubapi.RoleDefinition{}, false
}

// ensureRemoteCeiling makes the hub hold exactly one lever ceiling on userID:
// a system-scope access constraint on that one user whose maximum-permission
// set is perms. It returns the constraint's id.
//
// System scope because project.create is checked at system scope, and a
// system constraint applies at every scope (constraintScopeApplies), so the
// same set must also cover the project-scope permissions the lever-remote
// role grants — remoteCeilingPermissions includes them.
//
// Idempotent: a constraint with lever's name, this subject and this set is
// kept. One with the name but another subject (the hub user was re-created)
// or another set is deleted and created again; scion has no partial update
// without an If-Match header, and the window runs while the live hub is
// stopped, so there is no gap anyone can use. A recovery-disabled one is
// scion's offline-recovery state and is not lever's to undo.
func ensureRemoteCeiling(ctx context.Context, hc *hubapi.Client, email, userID string, perms []string, warn func(string, ...any)) (string, error) {
	name := remoteCeilingName(email)
	have, err := hc.AccessConstraintsNamed(ctx, name)
	if err != nil {
		return "", err
	}
	keep := ""
	var stale []hubapi.AccessConstraint
	for _, c := range have {
		if c.Status == "recovery_disabled" {
			return "", fmt.Errorf("access constraint %s (%s) is recovery-disabled; lever will not modify it", name, c.ID)
		}
		if keep == "" && c.ScopeType == hubapi.ConstraintScopeSystem && c.Subject.Kind == hubapi.ConstraintSubjectPrincipal &&
			c.Subject.PrincipalType == hubapi.PrincipalUser && c.Subject.PrincipalID == userID {
			d, err := hc.AccessConstraint(ctx, c.ID)
			if err != nil {
				return "", err
			}
			if hubapi.SamePermissions(d.MaximumPermissions, perms) {
				keep = c.ID
				continue
			}
			c.MaximumPermissions = d.MaximumPermissions
		}
		stale = append(stale, c)
	}
	for _, c := range stale {
		warn("remote web ceiling: access constraint %s (%s) does not match (subject %s, permissions %s); replacing it",
			name, c.ID, c.Subject.PrincipalID, strings.Join(c.MaximumPermissions, ","))
		if err := hc.DeleteAccessConstraint(ctx, c.ID, c.Revision); err != nil {
			return "", fmt.Errorf("deleting access constraint %s: %w", c.ID, err)
		}
	}
	if keep != "" {
		return keep, nil
	}
	got, err := hc.CreateAccessConstraint(ctx, hubapi.ConstraintDraft{
		Name:    name,
		Purpose: remoteCeilingPurpose,
		Subject: hubapi.ConstraintSubject{Kind: hubapi.ConstraintSubjectPrincipal,
			PrincipalType: hubapi.PrincipalUser, PrincipalID: userID},
		Scope:              hubapi.ConstraintScope{Type: hubapi.ConstraintScopeSystem},
		MaximumPermissions: perms,
	})
	if err != nil {
		return "", fmt.Errorf("creating access constraint %s: %w", name, err)
	}
	if got.ID == "" || !hubapi.SamePermissions(got.MaximumPermissions, perms) {
		return "", fmt.Errorf("access constraint %s after the write: id %q, permissions %s; want %s (the hub did not keep it)",
			name, got.ID, strings.Join(got.MaximumPermissions, ","), strings.Join(perms, ","))
	}
	return got.ID, nil
}
