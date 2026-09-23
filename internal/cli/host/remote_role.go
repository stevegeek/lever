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
// OIDC provider, so the web UI runs as a HUB USER (one per allowed login, see
// remoteproxy.HubUserEmails), not as the remote PAT. Since scion's
// permission rework a signed-in user holds only the system hub-members
// binding and no role on the instance project, so the chat and the /events
// stream answer 403. No lever token can fix that — scion denies UATs on
// hub-level resources, and role and binding admin is hub-admin only — so the
// grant runs in the bootstrap-token step's throwaway dev-auth window, where
// the dev identity is the super-admin.
const remoteWebRoleName = "lever-remote"

const remoteWebRoleDescription = "Managed by lever: the remote web UI's read, attach and message surface on this project."

// remoteRolePermissions is the permission set of the remote web role: the
// SAME surface as the remote PAT, derived from remotePATScopes so the two
// cannot drift. scion's registry names each permission as its UAT scope with
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
// when the record covers every email with the current permission set. It
// reads the record only, so `lever doctor` can use it without admin creds.
func remoteRoleReason(rec state.RemoteRoleRecord, found bool, emails, perms []string) string {
	if !found {
		return "no grant recorded"
	}
	if !hubapi.SamePermissions(rec.Permissions, perms) {
		return fmt.Sprintf("the role was granted with %s; this lever needs %s",
			strings.Join(rec.Permissions, ","), strings.Join(perms, ","))
	}
	var pending, unbound []string
	for _, e := range emails {
		if _, ok := rec.Bound[e]; ok {
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
	case len(pending) > 0:
		return "no hub user yet (never signed in): " + strings.Join(pending, ", ")
	}
	return ""
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
// Bindings for a user REMOVED from allowed_users are left in place: lever
// does not revoke here (see the remote-access guide).
func ensureRemoteWebRole(ctx context.Context, hc *hubapi.Client, projectKey string, emails []string, now time.Time, warn func(string, ...any)) (state.RemoteRoleRecord, error) {
	perms := remoteRolePermissions()
	projectID, err := hc.ProjectID(ctx, projectKey, fmt.Sprintf("http://127.0.0.1:%d", throwawayHubPort))
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}

	role, err := ensureRemoteRoleDefinition(ctx, hc, perms, warn)
	if err != nil {
		return state.RemoteRoleRecord{}, err
	}

	rec := state.RemoteRoleRecord{
		RoleID: role.ID, Permissions: perms, ProjectID: projectID,
		Bound: map[string]string{}, GrantedAt: now,
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
func ensureRemoteRoleDefinition(ctx context.Context, hc *hubapi.Client, perms []string, warn func(string, ...any)) (hubapi.RoleDefinition, error) {
	defs, err := hc.RoleDefinitions(ctx)
	if err != nil {
		return hubapi.RoleDefinition{}, err
	}
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
