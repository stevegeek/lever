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

// remoteWebRoleName is the custom project role lever binds the remote web UI's
// hub users to.
//
// The proxy sends the browser's hub session on every hub request, so the web
// UI runs as a hub user (one per allowed login, see
// remoteproxy.HubUserEmails); this role and the ceiling below are where the
// remote surface is narrowed. A signed-in user otherwise holds only the
// hub-members binding, so chat and /events answer 403. Role and binding admin
// is hub-admin only and scion denies UATs on hub-level resources, so the grant
// runs in the throwaway dev-auth window as the super-admin.
const remoteWebRoleName = "lever-remote"

const remoteWebRoleDescription = "Managed by lever: the remote web UI's read, attach and message surface on this project."

// remoteRolePermissions is the remote web role's permission set, derived from
// remotePATScopes so the two cannot drift; scion names each permission as its
// UAT scope with a dot for the colon.
//
// lifecycle adds agent.lifecycle, which a hub that has it uses to gate
// start/stop/suspend/restart/restore in the web UI. An older hub rejects the
// unknown permission, so it is added only when the hub lists agent:lifecycle.
func remoteRolePermissions(lifecycle bool) []string {
	scopes := remotePATScopes()
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = strings.Replace(s, ":", ".", 1)
	}
	// project.list is web-only and read-only: without it the SPA's project list
	// comes back empty. The remote PAT does not need it.
	out = append(out, "project.list")
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

// recordedLifecycle reports whether the record was granted with
// agent.lifecycle: the variant to keep when lever cannot ask the hub.
func recordedLifecycle(rec state.RemoteRoleRecord) bool {
	return slices.Contains(rec.Permissions, lifecyclePermission)
}

// scopeKnownFunc asks the jail's scion whether it knows a UAT scope
// (scion.Client.KnowsUATScope). nil means lever cannot ask.
type scopeKnownFunc func(ctx context.Context, scope string) (bool, error)

// remoteRoleLifecycle decides whether the remote role carries agent.lifecycle:
// what the jail's scion says or, when it cannot be asked, the recorded variant,
// so an unanswered probe never churns the grant.
func remoteRoleLifecycle(ctx context.Context, known scopeKnownFunc, rec state.RemoteRoleRecord) bool {
	if known != nil {
		if has, err := known(ctx, lifecycleScope); err == nil {
			return has
		}
	}
	return recordedLifecycle(rec)
}

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
// scion keeps constraint names unique per scope.
func remoteCeilingName(email string) string { return "lever-remote-ceiling:" + email }

// remoteCeilingPermissions is each ceiling's maximum-permission set: the
// hub-member role's permissions as the hub holds them, minus project.create,
// plus the lever-remote role's (or the ceiling would cut the surface the role
// grants). A constraint only reduces.
//
// It is read from the hub so a scion upgrade adding a hub-member permission is
// picked up the next time the window runs. Until then the ceiling withholds the
// new permission; deleting remote-role.json forces a recompute.
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

// remoteAccess is what the bootstrap-token step needs of the remote block.
type remoteAccess struct {
	Enabled bool
	Emails  []string
	// Contacts is the subset of Emails with tier contact: they get the contact
	// role, never the web role.
	Contacts []string
}

func remoteAccessFor(app *config.App) remoteAccess {
	if !app.RemoteEnabled() {
		return remoteAccess{}
	}
	ra := remoteAccess{Enabled: true, Emails: remoteproxy.HubUserEmails(app.Remote.Logins())}
	if c := app.Remote.LoginsWithTier(config.TierContact); len(c) > 0 {
		ra.Contacts = remoteproxy.HubUserEmails(c)
	}
	return ra
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

// remoteRoleFix is the repair for a pending user: the hub user exists only
// after its first sign-in, and only a dev-auth window can bind it. The window
// never opens beside a running container (windowBlocked), so otherwise only
// `lever stop` + `lever up` binds it.
const remoteRoleFix = "after the first sign-in from the phone, run `lever apply`; it binds the role only while " +
	"no container runs in the jail (the manager included); otherwise run `lever stop`, then `lever up`"

// remoteRoleReason says why the grant must (re)run, or "" when the record
// covers every email with the current permission set and a ceiling. It reads
// the record only, so `lever doctor` can use it without admin credentials.
func remoteRoleReason(rec state.RemoteRoleRecord, found bool, emails, contacts, perms []string) string {
	if !found {
		return "no grant recorded"
	}
	if r := contactRoleReason(rec, emails, contacts); r != "" {
		return r
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
		// Pending is what the hub said at the last grant; lever cannot ask again
		// without a dev-auth window.
		return "no hub user at the last grant (not signed in then): " + strings.Join(pending, ", ")
	}
	return ""
}

// contactRoleReason checks every bound email holds its tier's role, and the
// contact role and ceiling hold the current set. A tier change re-runs the
// grant.
func contactRoleReason(rec state.RemoteRoleRecord, emails, contacts []string) string {
	for _, e := range emails {
		if _, bound := rec.Bound[e]; !bound {
			continue
		}
		if want, got := slices.Contains(contacts, e), slices.Contains(rec.Contacts, e); want != got {
			return "the tier of " + e + " changed"
		}
	}
	if len(contacts) == 0 {
		return ""
	}
	perms := contactRolePermissions()
	if len(rec.Contacts) > 0 && !hubapi.SamePermissions(rec.ContactPermissions, perms) {
		return fmt.Sprintf("the contact role was granted with %s; this lever needs %s",
			strings.Join(rec.ContactPermissions, ","), strings.Join(perms, ","))
	}
	if len(rec.Contacts) > 0 && !hubapi.SamePermissions(rec.ContactCeilingPermissions, perms) {
		return fmt.Sprintf("the contacts' ceiling was written with %s; it must be exactly %s",
			strings.Join(rec.ContactCeilingPermissions, ","), strings.Join(perms, ","))
	}
	return ""
}

// remoteCeilingMissing: unlike the other reasons the web UI works, but it can
// create projects.
const remoteCeilingMissing = "no project-create ceiling yet, so the web UI can create projects"

// ceilingPermissionsFit reports whether a recorded ceiling keeps the role's
// permissions and withholds project.create. The rest comes from the hub-member
// role, which record-only readers cannot see.
func ceilingPermissionsFit(ceiling, role []string) bool {
	if len(ceiling) == 0 {
		return false
	}
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
// ~/.scion/dev-token. The value goes straight into a JailCurl env and is never
// printed.
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
// remoteRolePermissions, each email's hub user bound to it on the project, and
// a project-create ceiling per user. It runs as the dev identity (hc is the
// throwaway hub) and returns the record to persist.
//
// Idempotent. An email with no hub user yet (it appears on first sign-in) is
// listed in Pending and warned about. Bindings and ceilings of users removed
// from allowed_users are left in place: lever does not revoke here.
func ensureRemoteWebRole(ctx context.Context, hc *hubapi.Client, projectKey string, emails, contacts []string, lifecycle bool, now time.Time, warn func(string, ...any)) (state.RemoteRoleRecord, error) {
	perms := remoteRolePermissions(lifecycle)
	projectID, err := hc.ProjectID(ctx, projectKey, throwawayHubURL)
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}

	defs, err := hc.RoleDefinitions(ctx)
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}
	role, err := ensureProjectRole(ctx, hc, defs, remoteWebRoleName, remoteWebRoleDescription, perms, warn)
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
	var contactRole hubapi.RoleDefinition
	var contactCeiling []string
	if len(contacts) > 0 {
		cperms := contactRolePermissions()
		if contactRole, err = ensureProjectRole(ctx, hc, defs, contactRoleName, contactRoleDescription, cperms, warn); err != nil {
			return state.RemoteRoleRecord{}, err
		}
		// A contact's ceiling is its role alone: the fenced chat needs only identity
		// and agent.message, so hub-member's other reads are withheld too (defence
		// behind the proxy fence).
		contactCeiling = slices.Clone(cperms)
		rec.ContactRoleID, rec.ContactPermissions, rec.ContactCeilingPermissions = contactRole.ID, cperms, contactCeiling
	} else if r, ok := projectRoleNamed(defs, contactRoleName); ok {
		// No contacts now, but moving earlier ones back to operator needs the old
		// role's id.
		contactRole = r
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
		if u.ID == config.ScionDevUserID {
			// The dev row carries this login's email, which only scion's
			// unauthenticated workstation identity route can cause (an agent
			// renamed it). Binding or capping it would reshape lever's own
			// controller identity, and recording its id would bless the sign-in
			// the login driver refuses.
			warn("remote web role: the hub user for %s is scion's dev super-admin row, which an agent renamed to that address; "+
				"lever binds nothing to it and the login stays refused until the dev user's email is restored", email)
			rec.Pending = append(rec.Pending, email)
			continue
		}
		isContact := slices.Contains(contacts, email)
		grant, drop, name, userCeiling := role.ID, contactRole.ID, remoteWebRoleName, ceiling
		if isContact {
			grant, drop, name, userCeiling = contactRole.ID, role.ID, contactRoleName, contactCeiling
		}
		// The other tier's binding goes first: a login moved to contact must lose the
		// terminal and lifecycle it had.
		if err := dropProjectRoleBinding(ctx, hc, drop, u.ID, projectID); err != nil {
			if isContact {
				// A contact that keeps the web role keeps terminal and start/stop: fail the
				// apply rather than warn.
				return state.RemoteRoleRecord{}, fmt.Errorf("%w: removing contact %s from role %s: %w", errCeilingRemoved, email, remoteWebRoleName, err)
			}
			return state.RemoteRoleRecord{}, fmt.Errorf("removing hub user %s from its previous role: %w", email, err)
		}
		if err := ensureProjectRoleBinding(ctx, hc, grant, u.ID, projectID); err != nil {
			return state.RemoteRoleRecord{}, fmt.Errorf("binding hub user %s to role %s: %w", email, name, err)
		}
		rec.Bound[email] = u.ID
		if isContact {
			rec.Contacts = append(rec.Contacts, email)
		}
		id, err := ensureRemoteCeiling(ctx, hc, email, u.ID, userCeiling, warn)
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
// A matching constraint is kept. One with lever's name but another subject or
// set is deleted and recreated: names are unique per scope and scion has no
// partial update without If-Match, so the new one cannot come first. The live
// hub is stopped during the window, but a failed create leaves the user with
// no ceiling, so it returns errCeilingRemoved, which fails the apply. A
// recovery-disabled constraint is scion's offline-recovery state and is not
// lever's to undo.
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
	deleted := 0
	for _, c := range stale {
		warn("remote web ceiling: access constraint %s (%s) does not match (subject %s, permissions %s); replacing it",
			name, c.ID, c.Subject.PrincipalID, strings.Join(c.MaximumPermissions, ","))
		if err := hc.DeleteAccessConstraint(ctx, c.ID, c.Revision); err != nil {
			return "", ceilingRemovedIf(deleted > 0, fmt.Errorf("deleting access constraint %s: %w", c.ID, err))
		}
		deleted++
	}
	if keep != "" {
		return keep, nil
	}
	got, err := createCeiling(ctx, hc, name, userID, perms)
	if err != nil {
		return "", ceilingRemovedIf(deleted > 0, err)
	}
	return got, nil
}

// errCeilingRemoved marks a replacement that deleted the old constraint and
// then failed; grantRemoteWebRole fails the apply on it.
var errCeilingRemoved = errors.New("the grant left a remote user with more authority than its tier allows " +
	"(an old project-create ceiling deleted and the new one not written, or a contact still bound to the web role) " +
	"until a `lever apply` completes it")

func ceilingRemovedIf(removed bool, err error) error {
	if !removed {
		return err
	}
	return fmt.Errorf("%w: %w", errCeilingRemoved, err)
}

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
