package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/state"
)

// remoteWebRoleName is the custom project role lever binds the remote web UI's
// hub users to.
//
// The proxy sends the browser's hub session on every hub request, so the web
// UI runs as a hub user (one per allowed login, see
// remoteproxy.HubUserEmails); this role and the ceiling below are where the
// remote surface is narrowed. A signed-in user otherwise holds only the
// hub-members binding, so chat and /events answer 403. Role and binding admin
// is hub-admin only, so hub-identity grants it as lever-admin.
const remoteWebRoleName = "lever-remote"

const remoteWebRoleDescription = "Managed by lever: the remote web UI's read, attach and message surface on this project."

// remoteRolePermissions is the remote web role's permission set: read, list,
// attach and message, nothing that creates, deletes or reconfigures. It must
// never carry agent.manage, agent.create/delete, project.update or any secret
// permission. project.list keeps the SPA's project list from coming back
// empty.
//
// lifecycle adds agent.lifecycle, which a hub that has it uses to gate
// start/stop/suspend/restart/restore in the web UI. An older hub rejects the
// unknown permission, so it is added only when the hub lists agent:lifecycle.
func remoteRolePermissions(lifecycle bool) []string {
	out := []string{"agent.read", "agent.list", "project.read", "agent.attach", "agent.message", "project.list"}
	if lifecycle {
		out = append(out, lifecyclePermission)
	}
	return out
}

// lifecyclePermission is scion's agent.lifecycle permission and lifecycleScope
// its UAT scope, listed by /api/v1/auth/scopes on a hub that has it.
const (
	lifecyclePermission = "agent.lifecycle"
	lifecycleScope      = "agent:lifecycle"
)

// scopeKnownFunc asks the jail's scion whether it knows a UAT scope
// (scion.Client.KnowsUATScope). nil means lever cannot ask.
type scopeKnownFunc func(ctx context.Context, scope string) (bool, error)

// projectCreatePermission is the one hub-member permission the remote web
// users must not hold: a project a web user created would be owned by that
// user, outside every control lever keeps on the instance project.
const projectCreatePermission = "project.create"

// hubMemberRoleName is scion's system role every hub user holds through the
// hub-members group, which scion restores on every boot. It carries
// project.create and lever cannot unbind it, hence the ceiling.
const hubMemberRoleName = "hub-member"

const remoteCeilingPurpose = "Managed by lever: the remote web UI user may not create projects. " +
	"Maximum = hub-member minus project.create, plus the lever-remote project role."

// remoteCeilingName names lever's access constraint on one remote web user.
// scion keeps constraint names unique per scope, so a replacement goes under
// the second name (remoteCeilingNames) while the first still stands.
func remoteCeilingName(email string) string { return "lever-remote-ceiling:" + email }

func remoteCeilingNames(email string) []string {
	return []string{remoteCeilingName(email), remoteCeilingName(email) + ":b"}
}

// remoteCeilingPermissions is each ceiling's maximum-permission set: the
// hub-member role's permissions as the hub holds them, minus project.create,
// plus the lever-remote role's (or the ceiling would cut the surface the role
// grants). A constraint only reduces.
//
// It is read from the hub on every apply, so a scion upgrade adding a
// hub-member permission is picked up by the next apply.
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

// contactRoleName is the project role a contact-tier login's hub user gets
// instead of the web role: agent.message and nothing else.
//
// A direct chat needs only agent.message. agent.read and agent.list would show
// every agent's task prompt and config; project.read would stream every
// agent's replies and open the project message log; no agent.attach or
// agent.lifecycle. scion binds roles per project, so the per-agent limit is
// the proxy's contact fence (package remoteproxy).
const contactRoleName = "lever-remote-contact"

const contactRoleDescription = "Managed by lever: a remote contact may send direct chat to agents; the lever proxy limits which ones."

func contactRolePermissions() []string { return []string{"agent.message"} }

// remoteUser is one configured remote login's hub user.
type remoteUser struct {
	Email   string
	Contact bool
}

// remoteUsersFor is every login whose hub user lever invites and binds:
// allowed_users, or the placeholder operator when there are none, while
// remote access is on. With it off no login can sign in, so none is kept
// active; turning it back on reactivates and binds them before the proxy
// starts.
func remoteUsersFor(app *config.App) []remoteUser {
	if !app.RemoteEnabled() {
		return nil
	}
	contacts := remoteproxy.HubUserEmails(app.Remote.LoginsWithTier(config.TierContact))
	if len(app.Remote.LoginsWithTier(config.TierContact)) == 0 {
		contacts = nil
	}
	var out []remoteUser
	for _, e := range remoteproxy.HubUserEmails(app.Remote.Logins()) {
		e = strings.ToLower(e)
		out = append(out, remoteUser{Email: e, Contact: slices.ContainsFunc(contacts, func(c string) bool { return strings.EqualFold(c, e) })})
	}
	return out
}

// ensureRemoteUsers converges the remote logins. Every configured login's hub user
// is invited when absent, capped, then bound to its tier's role, before it
// first signs in; the first sign-in then activates that same row with its
// role and ceiling already in force. A login whose row is lever's own, the dev
// user, or not the one recorded for it is refused (only that login), and the
// proxy then refuses it too. A login removed from the config is suspended and
// unbound; suspension applies at once on the web and within the hub's 60 s
// cache on the API.
func (h *hubIdentity) ensureRemoteUsers(ctx context.Context, rec *state.HubIdentity, defs []hubapi.RoleDefinition, lifecycle bool) error {
	users := remoteUsersFor(h.app)
	if rec.Users == nil {
		rec.Users = map[string]string{}
	}
	if len(users) == 0 && len(rec.Users) == 0 {
		return nil
	}
	hc := h.admin
	perms := remoteRolePermissions(lifecycle)
	role, err := ensureProjectRole(ctx, hc, defs, remoteWebRoleName, remoteWebRoleDescription, perms, h.warn)
	if err != nil {
		return errHubIdentity("%w", err)
	}
	cperms := contactRolePermissions()
	contactRole, err := ensureProjectRole(ctx, hc, defs, contactRoleName, contactRoleDescription, cperms, h.warn)
	if err != nil {
		return errHubIdentity("%w", err)
	}
	hubMember, ok := systemRole(defs, hubMemberRoleName)
	if !ok {
		return errHubIdentity("the hub lists no %s system role, so lever cannot compute the project-create ceiling", hubMemberRoleName)
	}
	ceiling := remoteCeilingPermissions(hubMember.Permissions, perms)
	contactCeiling := slices.Clone(cperms)
	rr := state.RemoteRoleRecord{
		RoleID: role.ID, Permissions: perms, ProjectID: rec.ProjectID,
		Bound: map[string]string{}, Ceilings: map[string]string{},
		CeilingPermissions: ceiling, GrantedAt: h.guestNow(),
		ContactRoleID: contactRole.ID, ContactPermissions: cperms, ContactCeilingPermissions: contactCeiling,
	}
	configured := map[string]bool{}
	for _, u := range users {
		configured[u.Email] = true
		id, err := h.remoteUserID(ctx, u.Email, rec)
		if err != nil {
			if errors.Is(err, errLoginRefused) {
				h.warn("hub-identity: %v; that login is refused until this is resolved", err)
				continue
			}
			return err
		}
		if rec.Users[u.Email] != id {
			// Recorded before it is bound: an apply that fails later must still
			// find this hub user to suspend if the login is removed.
			rec.Users[u.Email] = id
			if err := h.st.SaveHubIdentity(*rec); err != nil {
				return errHubIdentity("%w", err)
			}
		}
		grant, drop, name, userCeiling := role.ID, contactRole.ID, remoteWebRoleName, ceiling
		if u.Contact {
			grant, drop, name, userCeiling = contactRole.ID, role.ID, contactRoleName, contactCeiling
		}
		// The ceiling first, then the bindings: a tier move never leaves the user
		// with more than either tier allows.
		cid, err := ensureRemoteCeiling(ctx, hc, u.Email, id, userCeiling, h.warn)
		if err != nil {
			return errHubIdentity("capping hub user %s: %w", u.Email, err)
		}
		if err := dropProjectRoleBinding(ctx, hc, drop, id, rec.ProjectID); err != nil {
			return errHubIdentity("%w: removing %s from its previous role: %w", errCeilingRemoved, u.Email, err)
		}
		if err := ensureProjectRoleBinding(ctx, hc, grant, id, rec.ProjectID); err != nil {
			return errHubIdentity("binding hub user %s to role %s: %w", u.Email, name, err)
		}
		rec.Users[u.Email] = id
		rr.Bound[u.Email], rr.Ceilings[u.Email] = id, cid
		if u.Contact {
			rr.Contacts = append(rr.Contacts, u.Email)
		}
	}
	for email, id := range rec.Users {
		if configured[email] {
			continue
		}
		if err := h.suspendRemoteUser(ctx, rec, email, id, role.ID, contactRole.ID); err != nil {
			return err
		}
	}
	if err := h.st.SaveRemoteRoleRecord(rr); err != nil {
		return errHubIdentity("persisting remote-role.json: %w", err)
	}
	return nil
}

// suspendRemovedLogins suspends and unbinds every recorded login the config
// no longer has (all of them with remote access off). It runs right after
// lever-admin is asserted, before anything else hub-identity does, so a
// failure later in the apply cannot leave a removed login active at the hub.
func (h *hubIdentity) suspendRemovedLogins(ctx context.Context, rec *state.HubIdentity, defs []hubapi.RoleDefinition) error {
	configured := map[string]bool{}
	for _, u := range remoteUsersFor(h.app) {
		configured[u.Email] = true
	}
	var roles []string
	if rec.ProjectID != "" {
		for _, name := range []string{remoteWebRoleName, contactRoleName} {
			if r, ok := projectRoleNamed(defs, name); ok {
				roles = append(roles, r.ID)
			}
		}
	}
	for email, id := range rec.Users {
		if configured[email] || id == "" {
			continue
		}
		if err := h.suspendRemoteUser(ctx, rec, email, id, roles...); err != nil {
			return err
		}
	}
	return nil
}

// errLoginRefused marks a login lever will not bind.
var errLoginRefused = errors.New("remote login refused")

// remoteUserID finds or invites the login's hub user and checks it is one
// lever may bind. A suspended row (the login was removed earlier and is back)
// is reactivated.
func (h *hubIdentity) remoteUserID(ctx context.Context, email string, rec *state.HubIdentity) (string, error) {
	if hubauth.IsReserved(email) {
		return "", fmt.Errorf("%w: %s is a reserved address", errLoginRefused, email)
	}
	u, found, err := h.admin.UserByEmail(ctx, email)
	if err != nil {
		return "", errHubIdentity("looking up hub user %s: %w", email, err)
	}
	if !found {
		inv, err := h.admin.InviteUser(ctx, email)
		switch {
		case err == nil:
			u = inv
		case errors.Is(err, hubapi.ErrUserExists):
			// Created between the lookup and the invite: read it back.
			if u, found, err = h.admin.UserByEmail(ctx, email); err != nil || !found {
				return "", errHubIdentity("hub user %s exists but cannot be read back: %v", email, err)
			}
		default:
			return "", errHubIdentity("inviting %s: %w", email, err)
		}
	}
	switch {
	case u.ID == devUserID || u.ID == rec.AdminID || u.ID == rec.ControllerID:
		return "", fmt.Errorf("%w: %s resolves to one of lever's own hub users (%s)", errLoginRefused, email, u.ID)
	case rec.Users[email] != "" && rec.Users[email] != u.ID:
		return "", fmt.Errorf("%w: %s is hub user %s, not the %s lever bound before", errLoginRefused, email, u.ID, rec.Users[email])
	}
	if u.Status == "suspended" {
		if _, err := h.admin.UpdateUser(ctx, u.ID, "", "active"); err != nil {
			return "", errHubIdentity("reactivating %s: %w", email, err)
		}
	}
	return u.ID, nil
}

// suspendRemoteUser suspends first: it fails closed sooner than the unbind.
//
// It never touches lever's own hub users, whatever the record says: every
// agent hangs off the controller (scion's dev user on a migrated hub), and a
// suspended controller, or one unbound from the project, holds them all.
func (h *hubIdentity) suspendRemoteUser(ctx context.Context, rec *state.HubIdentity, email, id string, roleIDs ...string) error {
	if id == devUserID || id == rec.AdminID || id == rec.ControllerID {
		h.warn("hub-identity: the record names lever's own hub user %s for removed login %s; lever leaves that user as it is (the proxy refuses the login)", id, email)
		return nil
	}
	projectID := rec.ProjectID
	u, ok, err := h.admin.GetUser(ctx, id)
	if err != nil {
		return errHubIdentity("reading removed login %s: %w", email, err)
	}
	if ok && u.Status != "suspended" {
		if _, err := h.admin.UpdateUser(ctx, id, "", "suspended"); err != nil {
			return errHubIdentity("suspending removed login %s: %w", email, err)
		}
		h.log("lever: remote login %s is not configured (or remote access is off); its hub user is suspended", email)
	}
	for _, r := range roleIDs {
		if err := dropProjectRoleBinding(ctx, h.admin, r, id, projectID); err != nil {
			return errHubIdentity("unbinding removed login %s: %w", email, err)
		}
	}
	return nil
}

func projectRoleNamed(defs []hubapi.RoleDefinition, name string) (hubapi.RoleDefinition, bool) {
	for _, d := range defs {
		if d.Name == name && d.ScopeType == hubapi.RoleScopeProject && !d.System {
			return d, true
		}
	}
	return hubapi.RoleDefinition{}, false
}

// dropProjectRoleBinding removes userID's direct binding to roleID, if any;
// roleID "" is a no-op.
func dropProjectRoleBinding(ctx context.Context, hc *hubapi.Client, roleID, userID, projectID string) error {
	if roleID == "" {
		return nil
	}
	have, err := hc.UserRoleBindings(ctx, userID)
	if err != nil {
		return err
	}
	for _, b := range have {
		if b.RoleDefinitionID == roleID && b.ScopeType == hubapi.RoleScopeProject && b.ScopeID == projectID {
			if err := hc.DeleteRoleBinding(ctx, b.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureProjectRole finds or creates a lever-managed project role and
// converges its permissions on perms. The hub's answer is checked, so a write
// the hub did not keep fails. A scion system role of the same name is never
// modified.
func ensureProjectRole(ctx context.Context, hc *hubapi.Client, defs []hubapi.RoleDefinition, name, description string, perms []string, warn func(string, ...any)) (hubapi.RoleDefinition, error) {
	var err error
	var role hubapi.RoleDefinition
	for _, d := range defs {
		if d.Name == name && d.ScopeType == hubapi.RoleScopeProject {
			role = d
			break
		}
	}
	want := hubapi.RoleDefinition{
		ID: role.ID, Name: name, Description: description,
		ScopeType: hubapi.RoleScopeProject, Permissions: perms,
	}
	switch {
	case role.ID == "":
		if role, err = hc.CreateRoleDefinition(ctx, want); err != nil {
			return hubapi.RoleDefinition{}, fmt.Errorf("creating role %s: %w", name, err)
		}
	case role.System:
		return hubapi.RoleDefinition{}, fmt.Errorf("role %s is a scion system role; lever will not modify it", name)
	case !hubapi.SamePermissions(role.Permissions, perms):
		warn("role %s has permissions %s; rewriting them to %s",
			name, strings.Join(role.Permissions, ","), strings.Join(perms, ","))
		if role, err = hc.UpdateRoleDefinition(ctx, want); err != nil {
			return hubapi.RoleDefinition{}, fmt.Errorf("updating role %s: %w", name, err)
		}
	default:
		return role, nil
	}
	if role.ID == "" || !hubapi.SamePermissions(role.Permissions, perms) {
		return hubapi.RoleDefinition{}, fmt.Errorf("role %s after the write: id %q, permissions %s; want %s (the hub did not keep it)",
			name, role.ID, strings.Join(role.Permissions, ","), strings.Join(perms, ","))
	}
	return role, nil
}

func ensureProjectRoleBinding(ctx context.Context, hc *hubapi.Client, roleID, userID, projectID string) error {
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

func systemRole(defs []hubapi.RoleDefinition, name string) (hubapi.RoleDefinition, bool) {
	for _, d := range defs {
		if d.Name == name && d.System && d.ScopeType == hubapi.RoleScopeSystem {
			return d, true
		}
	}
	return hubapi.RoleDefinition{}, false
}

// ensureRemoteCeiling makes the hub hold exactly one lever ceiling on userID:
// a system-scope access constraint with maximum-permission set perms. System
// scope because project.create is checked there; a system constraint applies
// at every scope, so perms must also cover the lever-remote role's permissions.
//
// A matching constraint is kept. Otherwise the new one is written first, under
// whichever of the user's two names is free, and only then are the old ones
// deleted: constraints only reduce, so for that moment the user holds the
// intersection, never more than either. A recovery-disabled constraint is
// scion's offline-recovery state and is not lever's to undo.
func ensureRemoteCeiling(ctx context.Context, hc *hubapi.Client, email, userID string, perms []string, warn func(string, ...any)) (string, error) {
	names := remoteCeilingNames(email)
	var have []hubapi.AccessConstraint
	used := map[string]bool{}
	for _, n := range names {
		cs, err := hc.AccessConstraintsNamed(ctx, n)
		if err != nil {
			return "", err
		}
		for _, c := range cs {
			if c.Name != n || used[c.Name+"\x00"+c.ID] {
				continue
			}
			used[c.Name], used[c.Name+"\x00"+c.ID] = true, true
			have = append(have, c)
		}
	}
	keep := ""
	var stale []hubapi.AccessConstraint
	for _, c := range have {
		if c.Status == "recovery_disabled" {
			return "", fmt.Errorf("access constraint %s (%s) is recovery-disabled; lever will not modify it", c.Name, c.ID)
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
	if keep == "" {
		free := ""
		for _, n := range names {
			if !used[n] {
				free = n
				break
			}
		}
		if free == "" {
			return "", fmt.Errorf("both of lever's ceiling names for %s are taken by constraints that do not match; delete one with scion and re-apply", email)
		}
		got, err := createCeiling(ctx, hc, free, userID, perms)
		if err != nil {
			return "", err
		}
		keep = got
	}
	for _, c := range stale {
		warn("remote web ceiling: access constraint %s (%s) does not match (subject %s, permissions %s); replaced",
			c.Name, c.ID, c.Subject.PrincipalID, strings.Join(c.MaximumPermissions, ","))
		if err := hc.DeleteAccessConstraint(ctx, c.ID, c.Revision); err != nil {
			return "", fmt.Errorf("deleting access constraint %s: %w", c.ID, err)
		}
	}
	return keep, nil
}

// errCeilingRemoved marks a tier move that left a contact bound to the web
// role; the apply fails on it.
var errCeilingRemoved = errors.New("the grant left a remote user with more authority than its tier allows " +
	"(a contact still bound to the web role) until a `lever apply` completes it")

// createCeiling checks the hub kept the ceiling it wrote.
func createCeiling(ctx context.Context, hc *hubapi.Client, name, userID string, perms []string) (string, error) {
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
