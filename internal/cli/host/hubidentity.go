package host

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// devUserID is scion's fixed ID for its dev-auth user (pkg/hub/devauth.go).
// On an instance an older lever brought up with dev-auth windows it owns the
// project, issued the PATs and created every agent; this lever keeps it as
// the controller.
const devUserID = "be67fbc9-c869-5d43-b15d-c28ca3e8d355"

// hubCacheWait bounds the wait for the hub's 60 s email-to-user cache after a
// rename, before an assertion for the old address resolves to the new row.
const hubCacheWait = 75 * time.Second

// hubCachePoll is how often that wait asks again.
const hubCachePoll = 5 * time.Second

// markerSlack is how far before this step the hub may have stamped an admin
// row and still count as created by it: guest and host clocks differ, and the
// signer only corrects for the offset read at the start of the run.
const markerSlack = 2 * time.Minute

// identityScion is what the hub-identity step needs of the jail's scion,
// driven with the controller PAT.
type identityScion interface {
	InitProject(ctx context.Context, dir string) error
	HubLink(ctx context.Context, dir string) error
	KnowsUATScope(ctx context.Context, scope string) (bool, error)
	List(ctx context.Context, project string) ([]scion.Agent, error)
}

// hubIdentity is the hub-identity apply step (design: steps A to G).
type hubIdentity struct {
	app       *config.App
	st        state.State
	jr        proc.Runner
	sc        identityScion
	admin     *hubapi.Client // lever-admin assertion
	ctl       *hubapi.Client // lever-controller assertion
	pat       *hubapi.Client // the controller PAT
	jailMount string
	keyFP     string
	// guestNow is the hub's clock as the signer sees it.
	guestNow func() time.Time
	reset    bool
	log      func(format string, args ...any)
	warn     func(format string, args ...any)
	sleep    func(ctx context.Context, d time.Duration) error
	// cacheWait and cachePoll: zero takes hubCacheWait and hubCachePoll.
	cacheWait, cachePoll time.Duration
}

func (h *hubIdentity) waitFor(ctx context.Context, d time.Duration) error {
	if h.sleep != nil {
		return h.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (h *hubIdentity) jailProject() string { return path.Clean(h.jailMount) }

// errHubIdentity prefixes every failure with the step name.
func errHubIdentity(format string, args ...any) error {
	return fmt.Errorf("hub-identity: "+format, args...)
}

// run converges the hub on lever's identities. It is idempotent, never lists
// containers, and is safe while agents run.
func (h *hubIdentity) run(ctx context.Context) error {
	rec, found, err := h.st.LoadHubIdentity()
	if err != nil {
		return errHubIdentity("%w", err)
	}
	if h.reset {
		h.log("lever: --reset-hub-identity: forgetting the recorded hub identities (%s) and bootstrapping the hub as new", h.st.HubIdentityFile())
		if err := h.forgetHub(); err != nil {
			return err
		}
		rec, found = state.HubIdentity{}, false
	}
	mode := rec.Mode
	if !found {
		mode = state.HubModeFresh
		if tok, _ := h.st.LoadControllerPAT(); tok != "" {
			// An older lever minted this PAT as scion's dev user in a dev-auth
			// window: the hub is migrated, not new.
			mode = state.HubModeMigrated
		}
	}
	stepStart := h.guestNow()

	// A: the admin.
	admin, err := h.ensureAdmin(ctx, mode, &rec)
	if err != nil {
		return err
	}
	// Flow 7: a record whose hub marker is provably gone means a new hub DB.
	if found && rec.HubMarker != "" {
		gone, err := h.markerGone(ctx, rec.HubMarker, admin, stepStart)
		if err != nil {
			return err
		}
		if gone {
			h.log("lever: WARNING: the jail's hub no longer holds lever's %s role (%s) and lever-admin was just created: "+
				"the hub database is new. Forgetting the old hub's records and bootstrapping it as new.", controllerRoleName, rec.HubMarker)
			if err := h.forgetHub(); err != nil {
				return err
			}
			rec, mode = state.HubIdentity{}, state.HubModeFresh
		}
	}
	rec.AdminID = admin.ID

	// A': only lever-admin is an admin, by role and by binding.
	demoted, err := h.adminHygiene(ctx, admin.ID)
	if err != nil {
		return err
	}

	// B: the controller.
	ctlUser, renamed, err := h.ensureController(ctx, mode, &rec)
	if err != nil {
		return err
	}
	rec.ControllerID = ctlUser.ID

	// C: the project.
	jp := h.jailProject()
	registered := false
	if rec.ProjectID == "" {
		if mode == state.HubModeFresh {
			id, err := h.registerProject(ctx, jp)
			if err != nil {
				return err
			}
			rec.ProjectID, registered = id, true
		} else {
			id, err := h.pat.ProjectID(ctx, path.Base(jp), scion.DefaultHubEndpoint)
			if err != nil {
				return errHubIdentity("finding the instance project: %w", err)
			}
			rec.ProjectID = id
		}
	}

	// D: the controller role, bound to the controller on the project.
	lifecycle := h.controllerLifecycle(ctx)
	defs, err := h.admin.RoleDefinitions(ctx)
	if err != nil {
		return errHubIdentity("listing roles: %w", err)
	}
	role, err := ensureProjectRole(ctx, h.admin, defs, controllerRoleName, controllerRoleDescription, controllerRolePermissions(lifecycle), h.warn)
	if err != nil {
		return errHubIdentity("%w", err)
	}
	if err := ensureProjectRoleBinding(ctx, h.admin, role.ID, ctlUser.ID, rec.ProjectID); err != nil {
		return errHubIdentity("binding the controller to role %s: %w", controllerRoleName, err)
	}
	rec.HubMarker = role.ID
	// Saved before the mint: a run that fails later must not lose which hub and
	// users the PAT below belongs to.
	if mode == state.HubModeMigrated && (demoted[devUserID] || renamed) {
		rec.ProbePending = true
	}
	rec.Version, rec.Mode, rec.KeyFingerprint = state.HubIdentityVersion, mode, h.keyFP
	if err := h.st.SaveHubIdentity(rec); err != nil {
		return errHubIdentity("%w", err)
	}

	// E: the PATs, minted by the controller.
	if err := h.mintPATs(ctx, rec.ProjectID, lifecycle); err != nil {
		return err
	}

	// C': link the local registration to the hub project.
	if registered {
		if err := h.sc.HubLink(ctx, jp); err != nil {
			return errHubIdentity("hub link: %w", err)
		}
	}

	// F: remote logins, invited and bound before they first sign in.
	if err := h.ensureRemoteUsers(ctx, &rec, defs, lifecycle); err != nil {
		return err
	}

	// G: records, then the probe after a migration changed the controller.
	if err := h.st.SaveHubIdentity(rec); err != nil {
		return errHubIdentity("%w", err)
	}
	if mode == state.HubModeMigrated && rec.ProbePending {
		if err := probeControllerAuthority(ctx, h.pat, h.sc, rec.ProjectID, jp, lifecycle); err != nil {
			return h.rollbackDemotion(ctx, err)
		}
		rec.ProbePending = false
		if err := h.st.SaveHubIdentity(rec); err != nil {
			return errHubIdentity("%w", err)
		}
		h.log("lever: hub identities migrated: lever-admin is the hub admin; the controller (%s) is a member and keeps the project, its PAT and every agent", devUserID)
	}
	return nil
}

// ensureAdmin asserts lever-admin and checks the row the hub resolved: an
// admin, and neither the dev user nor the controller. A rename of the dev
// user to lever-admin's address (any loopback caller can, through scion's
// system identity route) is repaired with the PAT (R1).
func (h *hubIdentity) ensureAdmin(ctx context.Context, mode string, rec *state.HubIdentity) (hubapi.User, error) {
	me, err := h.admin.Me(ctx)
	if err != nil {
		return hubapi.User{}, errHubIdentity("asserting lever-admin: %w", err)
	}
	if me.ID == devUserID {
		if mode != state.HubModeMigrated {
			return hubapi.User{}, errHubIdentity("lever-admin resolves to scion's dev user on a hub lever brought up without dev auth; " +
				"something started this hub with dev auth. Stop it, then run `lever up --reset-hub-identity` if the hub database is disposable")
		}
		h.log("lever: WARNING: scion's dev user carries lever-admin's address (renamed through the hub's identity route); restoring %s with the controller PAT", hubauth.ControllerEmail)
		if err := h.pat.SetDevIdentity(ctx, hubauth.ControllerEmail); err != nil {
			return hubapi.User{}, errHubIdentity("repairing the dev user's address: %w", err)
		}
		rec.Repairs = append(rec.Repairs, h.guestNow().UTC().Format(time.RFC3339)+" R1: dev user renamed to "+hubauth.AdminEmail+"; restored "+hubauth.ControllerEmail)
		if me, err = h.waitAdminRow(ctx); err != nil {
			return hubapi.User{}, err
		}
	}
	u, ok, err := h.admin.GetUser(ctx, me.ID)
	if err != nil || !ok {
		return hubapi.User{}, errHubIdentity("reading lever-admin's row %s: found=%v %v", me.ID, ok, err)
	}
	switch {
	case u.Role != "admin":
		return hubapi.User{}, errHubIdentity("lever-admin (%s) is not a hub admin (role %q); the hub was not started with lever's admin_emails", u.ID, u.Role)
	case rec.ControllerID != "" && u.ID == rec.ControllerID:
		return hubapi.User{}, errHubIdentity("lever-admin resolves to the controller's row %s; refusing", u.ID)
	}
	return u, nil
}

// waitAdminRow waits out the hub's email cache after R1.
func (h *hubIdentity) waitAdminRow(ctx context.Context) (hubapi.User, error) {
	wait, poll := h.cacheWait, h.cachePoll
	if wait == 0 {
		wait = hubCacheWait
	}
	if poll == 0 {
		poll = hubCachePoll
	}
	for waited := time.Duration(0); ; waited += poll {
		me, err := h.admin.Me(ctx)
		if err != nil {
			return hubapi.User{}, errHubIdentity("asserting lever-admin after the repair: %w", err)
		}
		if me.ID != devUserID {
			return me, nil
		}
		if waited >= wait {
			return hubapi.User{}, errHubIdentity("lever-admin still resolves to scion's dev user %s after the repair", wait)
		}
		if err := h.waitFor(ctx, poll); err != nil {
			return hubapi.User{}, err
		}
	}
}

// markerGone reports positive evidence of a new hub database: the role list
// answered without lever's role, and lever-admin's row was created by this
// run. Anything less is not evidence and fails closed.
func (h *hubIdentity) markerGone(ctx context.Context, marker string, admin hubapi.User, stepStart time.Time) (bool, error) {
	defs, err := h.admin.RoleDefinitions(ctx)
	if err != nil {
		return false, errHubIdentity("checking the hub marker: %w", err)
	}
	for _, d := range defs {
		if d.ID == marker {
			return false, nil
		}
	}
	if admin.Created.IsZero() || admin.Created.Before(stepStart.Add(-markerSlack)) {
		return false, errHubIdentity("the hub does not list lever's %s role %s, but lever-admin's row predates this run, so lever cannot tell a new hub "+
			"database from a damaged one. If the hub database was reset on purpose, run `lever up --reset-hub-identity`", controllerRoleName, marker)
	}
	return true, nil
}

// forgetHub drops every host record that belongs to the old hub.
func (h *hubIdentity) forgetHub() error {
	for _, rm := range []func() error{
		h.st.RemoveHubIdentity,
		h.st.RemoveRemotePAT,
		func() error { return removeIfPresent(h.st.ControllerPAT()) },
		func() error { return removeIfPresent(h.st.ControllerPATRecord()) },
		func() error { return removeIfPresent(h.st.RemoteRole()) },
	} {
		if err := rm(); err != nil {
			return errHubIdentity("forgetting the old hub's records: %w", err)
		}
	}
	return nil
}

// adminHygiene demotes every admin but lever-admin, by role and by binding
// (an admin promoted through the API keeps its binding through scion's own
// config demotion). It returns the demoted user IDs.
func (h *hubIdentity) adminHygiene(ctx context.Context, adminID string) (map[string]bool, error) {
	admins, err := h.admin.ListAdmins(ctx)
	if err != nil {
		return nil, errHubIdentity("listing admins: %w", err)
	}
	demoted := map[string]bool{}
	for _, a := range admins {
		if a.UserID == adminID {
			continue
		}
		h.log("lever: demoting hub admin %s (%s) to member: only lever-admin administers the hub", a.UserID, a.Email)
		if _, err := h.admin.UpdateUser(ctx, a.UserID, "member", ""); err != nil {
			return nil, errHubIdentity("demoting %s: %w", a.UserID, err)
		}
		demoted[a.UserID] = true
	}
	if len(demoted) == 0 {
		return demoted, nil
	}
	after, err := h.admin.ListAdmins(ctx)
	if err != nil {
		return nil, errHubIdentity("listing admins after the demotion: %w", err)
	}
	for _, a := range after {
		if a.UserID != adminID {
			return nil, errHubIdentity("%s (%s) is still a hub admin after the demotion (role %v, bindings %v)", a.UserID, a.Email, a.ByRole, a.BindingIDs)
		}
	}
	return demoted, nil
}

// ensureController makes lever-controller@lever.local resolve to the
// controller row. Fresh: an invited member. Migrated: scion's dev user,
// re-pinned to the address on every apply (R2), since any loopback caller can
// rename it; the address the hub held is never recorded.
func (h *hubIdentity) ensureController(ctx context.Context, mode string, rec *state.HubIdentity) (hubapi.User, bool, error) {
	renamed := false
	if mode == state.HubModeFresh {
		if _, ok, err := h.admin.UserByEmail(ctx, hubauth.ControllerEmail); err != nil {
			return hubapi.User{}, false, errHubIdentity("looking up the controller: %w", err)
		} else if !ok {
			if _, err := h.admin.InviteUser(ctx, hubauth.ControllerEmail); err != nil && !errors.Is(err, hubapi.ErrUserExists) {
				return hubapi.User{}, false, errHubIdentity("inviting the controller: %w", err)
			}
		}
	} else {
		dev, ok, err := h.admin.GetUser(ctx, devUserID)
		if err != nil || !ok {
			return hubapi.User{}, false, errHubIdentity("reading scion's dev user %s: found=%v %v", devUserID, ok, err)
		}
		if !strings.EqualFold(dev.Email, hubauth.ControllerEmail) {
			if err := h.admin.SetDevIdentity(ctx, hubauth.ControllerEmail); err != nil {
				h.warn("hub-identity: re-pinning the dev user's address as lever-admin failed (%v); trying the controller PAT", err)
				if err := h.pat.SetDevIdentity(ctx, hubauth.ControllerEmail); err != nil {
					return hubapi.User{}, false, errHubIdentity("re-pinning the dev user's address: %w", err)
				}
			}
			h.log("lever: the controller's hub address is now %s (it was %q)", hubauth.ControllerEmail, dev.Email)
			rec.Repairs = append(rec.Repairs, h.guestNow().UTC().Format(time.RFC3339)+" R2: re-pinned the dev user's address to "+hubauth.ControllerEmail)
			renamed = true
		}
	}
	me, err := h.ctl.Me(ctx)
	if err != nil {
		return hubapi.User{}, false, errHubIdentity("asserting lever-controller: %w", err)
	}
	if mode == state.HubModeMigrated && me.ID != devUserID {
		return hubapi.User{}, false, errHubIdentity("%s resolves to %s, not the controller (scion's dev user %s); another hub user holds the address",
			hubauth.ControllerEmail, me.ID, devUserID)
	}
	if rec.ControllerID != "" && me.ID != rec.ControllerID {
		return hubapi.User{}, false, errHubIdentity("%s resolves to %s, not the recorded controller %s; refusing", hubauth.ControllerEmail, me.ID, rec.ControllerID)
	}
	u, ok, err := h.admin.GetUser(ctx, me.ID)
	if err != nil || !ok {
		return hubapi.User{}, false, errHubIdentity("reading the controller's row %s: found=%v %v", me.ID, ok, err)
	}
	if u.Role == "admin" || u.Status != "active" || !strings.EqualFold(u.Email, hubauth.ControllerEmail) {
		return hubapi.User{}, false, errHubIdentity("the controller's row %s is role %q, status %q, address %q; want an active member at %s",
			u.ID, u.Role, u.Status, u.Email, hubauth.ControllerEmail)
	}
	return u, renamed, nil
}

// registerProject runs `scion init` and registers the local project on the
// hub as the controller, with the local project ID and slug: the body `scion
// hub link` sends, so the later link (with the controller PAT, which cannot
// register a project) takes scion's already-linked path.
func (h *hubIdentity) registerProject(ctx context.Context, jp string) (string, error) {
	if err := h.sc.InitProject(ctx, jp); err != nil {
		return "", errHubIdentity("scion init: %w", err)
	}
	id, slug, err := readProjectMarker(ctx, h.jr, jp)
	if err != nil {
		return "", errHubIdentity("%w", err)
	}
	p, _, err := h.ctl.RegisterProject(ctx, id, slug, jp)
	if err != nil {
		return "", errHubIdentity("registering project %s: %w", slug, err)
	}
	return p.ID, nil
}

// readProjectMarker reads the project ID and slug scion init wrote into the
// tree's marker file. lever trees are never git repositories
// (config.checkNonGitTree), so the marker is the one-file form.
func readProjectMarker(ctx context.Context, jr proc.Runner, jp string) (id, slug string, err error) {
	res, err := jr.Run(ctx, nil, "sh", "-c", `[ -f "$1" ] && [ ! -L "$1" ] && cat "$1"`, "_", path.Join(jp, ".scion"))
	if err != nil {
		return "", "", fmt.Errorf("reading the project marker %s/.scion: %w", jp, err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "project-id":
			id = strings.TrimSpace(v)
		case "project-slug":
			slug = strings.TrimSpace(v)
		}
	}
	if id == "" || slug == "" {
		return "", "", fmt.Errorf("the project marker %s/.scion names no project id or slug", jp)
	}
	return id, slug, nil
}

func (h *hubIdentity) controllerLifecycle(ctx context.Context) bool {
	rec, _, _ := h.st.LoadControllerPATRecord()
	var known scopeKnownFunc
	if h.sc != nil {
		known = h.sc.KnowsUATScope
	}
	return controllerLifecycle(ctx, known, rec)
}

// mintPATs mints the controller PAT (and the remote proxy's read PAT when
// remote access is on) as the controller, when patMintReason says so: save
// the token, then its record, then revoke the one it replaced, so a failure
// at any point leaves a working token on disk. The broker reads the PAT file
// per use, so this runs while agents run.
func (h *hubIdentity) mintPATs(ctx context.Context, projectID string, lifecycle bool) error {
	now := h.guestNow()
	mint := func(name string, scopes []string, load func() (string, error), loadRec func() (state.PATRecord, bool, error),
		save func(string) error, saveRec func(state.PATRecord) error) error {
		tok, _ := load()
		old, found, err := loadRec()
		if err != nil {
			return errHubIdentity("%w", err)
		}
		reason := patMintReason(tok, old, found, scopes, now)
		if reason == "" {
			return nil
		}
		if reason != patReasonAbsent {
			h.warn("hub-identity: re-minting %s: %s", name, reason)
		}
		secret, t, err := h.ctl.CreateToken(ctx, name, projectID, scopes, now.Add(patLifetime))
		if err != nil {
			return errHubIdentity("minting %s: %w", name, err)
		}
		if err := save(secret); err != nil {
			return errHubIdentity("persisting %s: %w", name, err)
		}
		if err := saveRec(state.PATRecord{ID: t.ID, Requested: scopes, Granted: t.Scopes, MintedAt: now, ExpiresAt: t.ExpiresAt}); err != nil {
			return errHubIdentity("persisting the %s record: %w", name, err)
		}
		if old.ID != "" && old.ID != t.ID {
			if err := h.ctl.RevokeToken(ctx, old.ID); err != nil && !errors.Is(err, hubapi.ErrNotFound) {
				h.warn("hub-identity: could not revoke the superseded %s token %s (it stays valid until %s): %v",
					name, old.ID, expiryText(old.ExpiresAt), err)
			}
		}
		return nil
	}
	if err := mint("lever-controller", controllerPATScopes(lifecycle), h.st.LoadControllerPAT, h.st.LoadControllerPATRecord,
		h.st.SaveControllerPAT, h.st.SaveControllerPATRecord); err != nil {
		return err
	}
	if !h.app.RemoteEnabled() {
		return nil
	}
	return mint("lever-remote", remotePATScopes(), h.st.LoadRemotePAT, h.st.LoadRemotePATRecord,
		h.st.SaveRemotePAT, h.st.SaveRemotePATRecord)
}

// patLifetime is just under scion's one-year maximum, clear of its
// "no later than now+1y" check on either clock.
const patLifetime = 360 * 24 * time.Hour

// rollbackDemotion restores the dev user's admin role when the controller
// lost an authority it needs, so the instance keeps working, and fails the
// apply with the probe's evidence: the fix is a lever-controller permission,
// not this rollback.
func (h *hubIdentity) rollbackDemotion(ctx context.Context, probeErr error) error {
	msg := fmt.Sprintf("the controller (scion's dev user) lacks an authority lever needs after its demotion: %v", probeErr)
	if _, err := h.admin.UpdateUser(ctx, devUserID, "admin", ""); err != nil {
		return errHubIdentity("%s; restoring its admin role also failed: %v", msg, err)
	}
	return errHubIdentity("%s. Its admin role was restored so agents keep working (the next apply demotes it and probes again); "+
		"report this, the fix is a permission on the %s role", msg, controllerRoleName)
}

// controllerActions are the agent actions lever performs with the controller
// PAT; lifecycle adds agent.lifecycle's action on a hub that has it.
func controllerActions(lifecycle bool) []string {
	a := []string{"read", "start", "stop", "message", "attach", "delete"}
	if lifecycle {
		a = append(a, "lifecycle")
	}
	return a
}

// probeControllerAuthority checks, without changing anything, that the
// controller PAT still does everything lever does with it: the hub's own
// capability read for every agent action, plus the project reads and writes'
// read halves and the CLI list.
func probeControllerAuthority(ctx context.Context, pat *hubapi.Client, sc identityScion, projectID, jp string, lifecycle bool) error {
	var failed []string
	if _, err := pat.Me(ctx); err != nil {
		failed = append(failed, "auth/me: "+err.Error())
	}
	if _, ok, err := pat.GetProject(ctx, projectID); err != nil || !ok {
		failed = append(failed, fmt.Sprintf("project %s: found=%v %v", projectID, ok, err))
	}
	if _, err := pat.SharedDirs(ctx, projectID); err != nil {
		failed = append(failed, "shared dirs: "+err.Error())
	}
	caps, err := pat.ProjectAgentCapabilities(ctx, projectID)
	if err != nil {
		failed = append(failed, "agent capabilities: "+err.Error())
	} else {
		if !slices.Contains(caps.Scope, "create") {
			failed = append(failed, fmt.Sprintf("agent create (scope actions %v)", caps.Scope))
		}
		for slug, acts := range caps.Agents {
			for _, want := range controllerActions(lifecycle) {
				if !slices.Contains(acts, want) {
					failed = append(failed, fmt.Sprintf("agent %s: %s (actions %v)", slug, want, acts))
				}
			}
		}
	}
	if _, err := sc.List(ctx, jp); err != nil {
		failed = append(failed, "scion list: "+err.Error())
	}
	if len(failed) > 0 {
		return fmt.Errorf("controller PAT probe failed: %s", strings.Join(failed, "; "))
	}
	return nil
}
