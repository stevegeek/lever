package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// devUserID is scion's fixed ID for its dev-auth user (pkg/hub/devauth.go).
// On a migrated hub it owns the project, issued the PATs and created every
// agent, so it stays the controller: scion grants an agent's owner and its
// attested ancestry (whose root is this row) by user ID, and has no way to
// re-own an agent, so moving any of that would cut agents off.
//
// Any loopback caller, an agent included, can rename this row through scion's
// system identity route. So lever never relies on its address except for the
// one thing that needs it, an assertion as the controller to mint or revoke
// its token, and identifies the row by ID everywhere else.
const devUserID = config.ScionDevUserID

// controllerAssertAttempts bounds the re-pin and assert retries on a migrated
// hub; controllerAssertFresh is how long a resolved controller assertion is
// trusted, well inside scion's 60 s email-to-user cache that keeps it on the
// row it resolved to.
const (
	controllerAssertAttempts = 5
	controllerAssertFresh    = 30 * time.Second
)

// hubCacheWait bounds the wait for the hub's 60 s email-to-user cache after a
// rename, before an assertion for the old address resolves to the new row.
const hubCacheWait = 75 * time.Second

// hubCachePoll is how often that wait asks again.
const hubCachePoll = 5 * time.Second

// markerSlack is how far before this run's start the hub may have stamped an
// admin row and still count as created by it: guest and host clocks differ, and the
// signer only corrects for the offset read at the start of the run.
const markerSlack = 2 * time.Minute

// identityScion is what the hub-identity step needs of the jail's scion,
// driven with the controller PAT.
type identityScion interface {
	InitProject(ctx context.Context, dir string) error
	HubLink(ctx context.Context, dir string) error
	KnowsUATScope(ctx context.Context, scope string) (bool, error)
	List(ctx context.Context, project string) ([]scion.Agent, error)
	ConfigGetProject(ctx context.Context, dir, key string) (string, error)
}

// hubIdentity is the hub-identity apply step.
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
	// runStart is guestNow when the apply began; zero when unknown. Steps
	// before this one assert as lever-admin too (the scion-server step's
	// broker check), so a new hub database can get lever-admin's row before
	// this step starts.
	runStart time.Time
	reset    bool
	// restartHub stops and starts the live hub and waits for its broker; only
	// a pinned-off scion experiment found on needs it.
	restartHub func(ctx context.Context) error
	// ssh is the `lever ssh` decision for this apply (sshGate); the zero
	// value keeps conduit off.
	ssh sshGate
	// linkBroker makes the project at a jail path list a runtime broker
	// (ensureProjectProvider); nil skips it.
	linkBroker func(ctx context.Context, jp string) error
	log        func(format string, args ...any)
	warn       func(format string, args ...any)
	sleep      func(ctx context.Context, d time.Duration) error
	// cacheWait and cachePoll: zero takes hubCacheWait and hubCachePoll.
	cacheWait, cachePoll time.Duration
	// ctlAsserted is when an assertion as the controller last resolved to the
	// controller's row in this run.
	ctlAsserted time.Time
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
	// The project lever last registered, for checking the tree's marker on a
	// re-bootstrap: the tree is the manager's workspace.
	prevProject := rec.ProjectID
	if h.reset {
		// Checked before anything is revoked or forgotten.
		if _, err := h.checkProjectMarker(ctx, h.jailProject(), prevProject); err != nil {
			return err
		}
		h.log("lever: --reset-hub-identity: forgetting the recorded hub identities (%s) and bootstrapping the hub as new", h.st.HubIdentityFile())
		h.revokeRecordedPATs(ctx)
		if err := h.forgetHub(); err != nil {
			return err
		}
		rec, found = state.HubIdentity{}, false
	}
	mode := rec.Mode
	if !found {
		mode = state.HubModeFresh
		if tok, _ := h.st.LoadControllerPAT(); tok != "" {
			// A PAT with no identity record was minted as scion's dev user: the
			// hub has a dev row that owns everything.
			mode = state.HubModeMigrated
		}
	}
	runFrom := h.guestNow()
	if !h.runStart.IsZero() && h.runStart.Before(runFrom) {
		runFrom = h.runStart
	}

	admin, err := h.ensureAdmin(ctx, mode, &rec)
	if err != nil {
		return err
	}
	if found && rec.HubMarker != "" {
		gone, err := h.markerGone(ctx, rec.HubMarker, admin, runFrom)
		if err != nil {
			return err
		}
		if gone {
			if _, err := h.checkProjectMarker(ctx, h.jailProject(), prevProject); err != nil {
				return err
			}
			h.log("lever: WARNING: the jail's hub no longer holds lever's %s role (%s) and lever-admin was just created: "+
				"the hub database is new. Forgetting the old hub's records and bootstrapping it as new.", controllerRoleName, rec.HubMarker)
			if err := h.forgetHub(); err != nil {
				return err
			}
			rec, mode = state.HubIdentity{}, state.HubModeFresh
		}
	}
	rec.AdminID = admin.ID
	if mode == state.HubModeFresh && !found {
		// A hub with scion's dev user is not new, whatever the host files say
		// (a deleted PAT, or a reset of a hub whose database was kept): that row
		// owns the project and every agent, so it becomes the controller.
		if _, dev, err := h.admin.GetUser(ctx, devUserID); err != nil {
			return errHubIdentity("looking for scion's dev user: %w", err)
		} else if dev {
			h.log("lever: the hub has scion's dev user although lever holds no record of it: treating the hub as migrated")
			mode = state.HubModeMigrated
		}
	}
	// owe records, before the change, that a migration changed the controller:
	// a run that stops after it still leaves the next one a probe to run.
	owe := func() error {
		if mode != state.HubModeMigrated || rec.ProbePending {
			return nil
		}
		rec.ProbePending = true
		rec.Version, rec.Mode, rec.KeyFingerprint = state.HubIdentityVersion, mode, h.keyFP
		if err := h.st.SaveHubIdentity(rec); err != nil {
			return errHubIdentity("%w", err)
		}
		return nil
	}
	if !found {
		if err := owe(); err != nil {
			return err
		}
	}

	defs, err := h.admin.RoleDefinitions(ctx)
	if err != nil {
		return errHubIdentity("listing roles: %w", err)
	}
	// Revocation comes first and needs only lever-admin: no later step, and
	// nothing an agent can disturb (the controller's address), stands between
	// a removed login and its suspension.
	if err := h.suspendRemovedLogins(ctx, &rec, defs); err != nil {
		return err
	}
	conduitOn, err := ensureHubExperiments(ctx, experimentsPin{
		admin: h.admin, st: h.st, known: h.scopeKnown(), now: h.guestNow,
		restartHub: h.restartHub, conduitOn: h.ssh.on, conduitPolicyLive: h.ssh.policyLive,
		log: h.log, warn: h.warn,
	})
	if err != nil {
		return errHubIdentity("%w", err)
	}

	// The registry route takes any loopback caller, and on a hub with DB-backed
	// settings an agent's value outranks the config-registry step's
	// settings.yaml and applies at the next agent start: the next agent would
	// run an image from the agent's registry.
	if err := h.admin.SetImageRegistry(ctx, apply.ImageRegistry); err != nil {
		return errHubIdentity("setting the hub's agent image registry to %s as lever-admin: %w", apply.ImageRegistry, err)
	}
	// The runtime route is open the same way and lasts the same way; another
	// runtime fails every agent start.
	if err := h.admin.SetRuntime(ctx, apply.AgentRuntime); err != nil {
		return errHubIdentity("setting the hub's container runtime to %s as lever-admin: %w", apply.AgentRuntime, err)
	}

	if _, err := h.adminHygiene(ctx, admin.ID, owe); err != nil {
		return err
	}

	ctlUser, err := h.ensureController(ctx, mode, &rec, owe)
	if err != nil {
		return err
	}
	rec.ControllerID = ctlUser.ID

	jp := h.jailProject()
	registered := false
	if rec.ProjectID == "" {
		if mode == state.HubModeFresh {
			id, err := h.registerProject(ctx, jp, prevProject)
			if err != nil {
				return err
			}
			rec.ProjectID, registered = id, true
		} else {
			id, err := h.migratedProjectID(ctx, path.Base(jp))
			if err != nil {
				return err
			}
			rec.ProjectID = id
		}
	}

	lifecycle := h.controllerLifecycle(ctx)
	perms := controllerRolePermissions(lifecycle)
	if conduitOn {
		// `lever ssh` mints its tunnel token as the controller, and a token
		// carries only what its issuer holds on the project.
		perms = append(perms, portAccessPermission)
	}
	role, err := ensureProjectRole(ctx, h.admin, defs, controllerRoleName, controllerRoleDescription, perms, h.warn)
	if err != nil {
		return errHubIdentity("%w", err)
	}
	if err := ensureProjectRoleBinding(ctx, h.admin, role.ID, ctlUser.ID, rec.ProjectID); err != nil {
		return errHubIdentity("binding the controller to role %s: %w", controllerRoleName, err)
	}
	rec.HubMarker = role.ID
	// Saved before the mint: a run that fails later must not lose which hub and
	// users the PAT below belongs to.
	rec.Version, rec.Mode, rec.KeyFingerprint = state.HubIdentityVersion, mode, h.keyFP
	if err := h.st.SaveHubIdentity(rec); err != nil {
		return errHubIdentity("%w", err)
	}

	if err := h.mintPATs(ctx, mode, rec.ProjectID, controllerPATScopes(lifecycle, h.controllerInbox(ctx))); err != nil {
		return err
	}
	if !conduitOn {
		h.revokeSSHTokens(ctx, mode)
	}
	h.revokeUnrecordedTokens(ctx, mode, rec.ControllerID)

	// The PAT cannot register a project, so the link comes after the REST
	// registration and the mint, and takes scion's already-linked path.
	if registered {
		// The link's agent sync needs a runtime broker on the project.
		if h.linkBroker != nil {
			if err := h.linkBroker(ctx, jp); err != nil {
				return errHubIdentity("%w", err)
			}
		}
		if err := h.sc.HubLink(ctx, jp); err != nil {
			return errHubIdentity("hub link: %w", err)
		}
	}

	if err := h.ensureRemoteUsers(ctx, &rec, defs, lifecycle); err != nil {
		return err
	}

	if err := h.st.SaveHubIdentity(rec); err != nil {
		return errHubIdentity("%w", err)
	}
	if mode == state.HubModeMigrated && rec.ProbePending {
		patRec, _, _ := h.st.LoadControllerPATRecord()
		if err := probeControllerAuthority(ctx, h.pat, h.sc, rec.ProjectID, jp, lifecycle, patRec); err != nil {
			// Never re-promoted: an admin role granted through the API outlives
			// scion's config reconcile, and the controller's PAT is the broker's
			// credential. The controller stays a member, the probe stays owed,
			// and the operator decides.
			if errors.Is(err, errAuthorityMissing) {
				return errHubIdentity("the controller (scion's dev user) lacks an authority lever needs after its demotion: %v. "+
					"It stays a member and the next apply probes again; until then lever's agent operations may fail. "+
					"Report this: the fix is a permission on the %s role", err, controllerRoleName)
			}
			return errHubIdentity("%v", err)
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
			return hubapi.User{}, errHubIdentity("lever-admin still resolves to scion's dev user %s after the repair: something keeps renaming the dev user "+
				"to lever-admin's address through the hub's identity route, most likely an agent. Remote login removals already took effect at the proxy. "+
				"Stop the agents (`lever stop`), then run `lever apply`", wait)
		}
		if err := h.waitFor(ctx, poll); err != nil {
			return hubapi.User{}, err
		}
	}
}

// markerGone reports positive evidence of a new hub database: the role list
// answered without lever's role, and lever-admin's row was created by this
// run. Anything less is not evidence and fails closed.
func (h *hubIdentity) markerGone(ctx context.Context, marker string, admin hubapi.User, runFrom time.Time) (bool, error) {
	defs, err := h.admin.RoleDefinitions(ctx)
	if err != nil {
		return false, errHubIdentity("checking the hub marker: %w", err)
	}
	for _, d := range defs {
		if d.ID == marker {
			return false, nil
		}
	}
	if admin.Created.IsZero() || admin.Created.Before(runFrom.Add(-markerSlack)) {
		return false, errHubIdentity("the hub does not list lever's %s role %s, but lever-admin's row predates this run, so lever cannot tell a new hub "+
			"database from a damaged one. If the hub database was reset on purpose, run `lever up --reset-hub-identity`", controllerRoleName, marker)
	}
	return true, nil
}

// forgetHub drops every host record that belongs to the old hub.
func (h *hubIdentity) forgetHub() error {
	// The record goes last: while it remains, a failed forget is retried.
	for _, rm := range []func() error{
		h.st.RemoveRemotePAT,
		func() error { return removeIfPresent(h.st.ControllerPAT()) },
		func() error { return removeIfPresent(h.st.ControllerPATRecord()) },
		func() error { return removeIfPresent(h.st.RemoteRole()) },
		func() error { return removeIfPresent(h.st.HubExperiments()) },
		h.st.RemoveHubIdentity,
	} {
		if err := rm(); err != nil {
			return errHubIdentity("forgetting the old hub's records: %w", err)
		}
	}
	return nil
}

// revokeRecordedPATs revokes, best effort, the tokens lever recorded for the
// hub it is about to forget: a reset of a hub whose database was in fact kept
// would otherwise leave them valid and unrecorded.
func (h *hubIdentity) revokeRecordedPATs(ctx context.Context) {
	for _, load := range []func() (state.PATRecord, bool, error){h.st.LoadControllerPATRecord, h.st.LoadRemotePATRecord} {
		r, found, err := load()
		if err != nil || !found || r.ID == "" {
			continue
		}
		switch err := h.ctl.RevokeToken(ctx, r.ID); {
		case err == nil:
			h.log("lever: revoked the old hub token %s before forgetting it", r.ID)
		case errors.Is(err, hubapi.ErrNotFound):
		default:
			h.warn("hub-identity: could not revoke the old hub token %s before forgetting it (it stays valid until %s if the hub database was kept): %v",
				r.ID, expiryText(r.ExpiresAt), err)
		}
	}
}

// adminHygiene demotes every admin but lever-admin, by role and by binding
// (an admin promoted through the API keeps its binding through scion's own
// config demotion). It returns the demoted user IDs.
func (h *hubIdentity) adminHygiene(ctx context.Context, adminID string, beforeDevChange func() error) (map[string]bool, error) {
	admins, err := h.admin.ListAdmins(ctx)
	if err != nil {
		return nil, errHubIdentity("listing admins: %w", err)
	}
	demoted := map[string]bool{}
	for _, a := range admins {
		if a.UserID == adminID {
			continue
		}
		if a.UserID == devUserID {
			if err := beforeDevChange(); err != nil {
				return nil, err
			}
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

// ensureController finds the controller row and checks it: an active
// member. Fresh: an invited member at lever-controller@lever.local, found by
// asserting that address. Migrated: scion's dev user, found by ID through the
// controller PAT, so a rename cannot stop an apply that mints nothing; its
// address is re-pinned on every apply (R2) for the sender label, best effort.
func (h *hubIdentity) ensureController(ctx context.Context, mode string, rec *state.HubIdentity, beforeDevChange func() error) (hubapi.User, error) {
	var id string
	if mode == state.HubModeFresh {
		if _, ok, err := h.admin.UserByEmail(ctx, hubauth.ControllerEmail); err != nil {
			return hubapi.User{}, errHubIdentity("looking up the controller: %w", err)
		} else if !ok {
			if _, err := h.admin.InviteUser(ctx, hubauth.ControllerEmail); err != nil && !errors.Is(err, hubapi.ErrUserExists) {
				return hubapi.User{}, errHubIdentity("inviting the controller: %w", err)
			}
		}
		me, err := h.ctl.Me(ctx)
		if err != nil {
			return hubapi.User{}, errHubIdentity("asserting lever-controller: %w", err)
		}
		id = me.ID
	} else {
		if err := h.repinController(ctx, rec, beforeDevChange); err != nil {
			return hubapi.User{}, err
		}
		var err error
		if id, err = h.migratedControllerID(ctx); err != nil {
			return hubapi.User{}, err
		}
	}
	if rec.ControllerID != "" && id != rec.ControllerID {
		return hubapi.User{}, errHubIdentity("the controller resolves to %s, not the recorded controller %s; refusing", id, rec.ControllerID)
	}
	u, ok, err := h.admin.GetUser(ctx, id)
	if err != nil || !ok {
		return hubapi.User{}, errHubIdentity("reading the controller's row %s: found=%v %v", id, ok, err)
	}
	if u.Role == "admin" || u.Status != "active" || mode == state.HubModeFresh && !strings.EqualFold(u.Email, hubauth.ControllerEmail) {
		return hubapi.User{}, errHubIdentity("the controller's row %s is role %q, status %q, address %q; want an active member at %s",
			u.ID, u.Role, u.Status, u.Email, hubauth.ControllerEmail)
	}
	if !strings.EqualFold(u.Email, hubauth.ControllerEmail) {
		h.warn("hub-identity: the controller's address is %q right after lever re-pinned it to %s: something is renaming scion's dev user "+
			"through the hub's identity route, most likely an agent. lever's sender label follows that address until it is re-pinned", u.Email, hubauth.ControllerEmail)
	}
	return u, nil
}

// repinController restores the dev user's address (R2) when it differs.
func (h *hubIdentity) repinController(ctx context.Context, rec *state.HubIdentity, beforeDevChange func() error) error {
	dev, ok, err := h.admin.GetUser(ctx, devUserID)
	if err != nil || !ok {
		return errHubIdentity("reading scion's dev user %s: found=%v %v", devUserID, ok, err)
	}
	if strings.EqualFold(dev.Email, hubauth.ControllerEmail) {
		return nil
	}
	if err := beforeDevChange(); err != nil {
		return err
	}
	if err := h.admin.SetDevIdentity(ctx, hubauth.ControllerEmail); err != nil {
		h.warn("hub-identity: re-pinning the dev user's address as lever-admin failed (%v); trying the controller PAT", err)
		if err := h.pat.SetDevIdentity(ctx, hubauth.ControllerEmail); err != nil {
			return errHubIdentity("re-pinning the dev user's address: %w", err)
		}
	}
	h.log("lever: the controller's hub address is now %s (it was %q)", hubauth.ControllerEmail, dev.Email)
	rec.Repairs = append(rec.Repairs, h.guestNow().UTC().Format(time.RFC3339)+" R2: re-pinned the dev user's address to "+hubauth.ControllerEmail)
	return nil
}

// migratedControllerID is the controller's ID as its PAT reports it, which
// no rename changes. Without a working PAT (none on disk, or the hub refuses
// it) the controller is asserted instead.
func (h *hubIdentity) migratedControllerID(ctx context.Context) (string, error) {
	if tok, _ := h.st.LoadControllerPAT(); tok != "" {
		me, err := h.pat.Me(ctx)
		switch {
		case err == nil && me.ID != devUserID:
			return "", errHubIdentity("the controller PAT belongs to hub user %s, not the controller (scion's dev user %s); refusing", me.ID, devUserID)
		case err == nil:
			return me.ID, nil
		case !refused(err):
			return "", errHubIdentity("reading the controller through its PAT: %w", err)
		}
		h.warn("hub-identity: the hub refused the controller PAT (%v); identifying the controller by assertion", err)
	}
	me, err := h.assertController(ctx, state.HubModeMigrated)
	if err != nil {
		return "", err
	}
	return me.ID, nil
}

// errControllerContested is a controller assertion that kept missing the dev
// user's row because its address kept changing.
var errControllerContested = errors.New("lever-controller's address does not stay on the controller")

// assertController makes an assertion as the controller resolve to the
// controller's row, before a call that only that assertion can make (a token
// mint or revoke, a project registration). On a migrated hub the address is
// contested: anything that reaches the hub's identity route can rename the
// dev user between lever's re-pin and its assertion. So the re-pin and the
// assertion are retried together a few times; once the assertion resolves to
// the dev user, scion's email cache keeps it there for a minute, which is
// what the calls that follow rely on.
func (h *hubIdentity) assertController(ctx context.Context, mode string) (hubapi.User, error) {
	if mode != state.HubModeMigrated {
		me, err := h.ctl.Me(ctx)
		if err != nil {
			return hubapi.User{}, errHubIdentity("asserting lever-controller: %w", err)
		}
		h.ctlAsserted = h.guestNow()
		return me, nil
	}
	if !h.ctlAsserted.IsZero() && h.guestNow().Sub(h.ctlAsserted) < controllerAssertFresh {
		return hubapi.User{ID: devUserID}, nil
	}
	var last error
	for range controllerAssertAttempts {
		dev, ok, err := h.admin.GetUser(ctx, devUserID)
		if err != nil || !ok {
			return hubapi.User{}, errHubIdentity("reading scion's dev user %s: found=%v %v", devUserID, ok, err)
		}
		if !strings.EqualFold(dev.Email, hubauth.ControllerEmail) {
			if err := h.admin.SetDevIdentity(ctx, hubauth.ControllerEmail); err != nil {
				return hubapi.User{}, errHubIdentity("re-pinning the dev user's address: %w", err)
			}
		}
		me, err := h.ctl.Me(ctx)
		switch {
		case err == nil && me.ID == devUserID:
			h.ctlAsserted = h.guestNow()
			return me, nil
		case err == nil:
			return hubapi.User{}, errHubIdentity("%s resolves to %s, not the controller (scion's dev user %s); another hub user holds the address",
				hubauth.ControllerEmail, me.ID, devUserID)
		case !refused(err):
			return hubapi.User{}, errHubIdentity("asserting lever-controller: %w", err)
		}
		last = err
	}
	return hubapi.User{}, errHubIdentity("%w (scion's dev user %s) long enough to mint or revoke its token, after %d attempts (last: %v). "+
		"Something is renaming the dev user through the hub's identity route, most likely an agent. "+
		"Remote login removals and suspensions already took effect; the existing controller token keeps working. "+
		"Stop the agents (`lever stop`), then run `lever apply`",
		errControllerContested, devUserID, controllerAssertAttempts, last)
}

// migratedProjectID finds the instance project on a migrated hub, through
// the controller PAT when it can.
func (h *hubIdentity) migratedProjectID(ctx context.Context, name string) (string, error) {
	if tok, _ := h.st.LoadControllerPAT(); tok != "" {
		id, err := h.pat.ProjectID(ctx, name, scion.DefaultHubEndpoint)
		if err == nil {
			return id, nil
		}
		if !refused(err) {
			return "", errHubIdentity("finding the instance project: %w", err)
		}
	}
	if _, err := h.assertController(ctx, state.HubModeMigrated); err != nil {
		return "", err
	}
	id, err := h.ctl.ProjectID(ctx, name, scion.DefaultHubEndpoint)
	if err != nil {
		return "", errHubIdentity("finding the instance project: %w", err)
	}
	return id, nil
}

// refused reports a hub answer that refused the credential (401 or 403), as
// opposed to one lever could not read.
func refused(err error) bool {
	var ae *hubapi.APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized)
}

// registerProject runs `scion init` and registers the local project on the
// hub as the controller, with the local project ID and slug: the body `scion
// hub link` sends, so the later link (with the controller PAT, which cannot
// register a project) takes scion's already-linked path.
//
// The marker lives in the tree, which is the manager's workspace. On a
// re-bootstrap (prev, the project lever registered before, is known) a
// marker naming another project is refused rather than registered: an agent
// could have written it. A marker that changes during `scion init` is
// refused too.
func (h *hubIdentity) registerProject(ctx context.Context, jp, prev string) (string, error) {
	before, err := h.checkProjectMarker(ctx, jp, prev)
	if err != nil {
		return "", err
	}
	if prev == "" {
		// A marker left by an instance whose hub is gone (`lever destroy`, then
		// `lever up` on the same tree). scion init on a new VM keeps it but
		// records another local project ID, and the controller PAT's link then
		// looks up that one, which the hub does not know.
		//
		// An unreadable marker goes too: readProjectMarker refuses a symlink,
		// and scion init writes the new marker through one.
		if before != "" {
			h.log("lever: the tree's project marker names project %s from an earlier hub; scion init writes a new one", before)
		}
		if err := removeJailFile(ctx, h.jr, path.Join(jp, ".scion")); err != nil {
			return "", errHubIdentity("%w", err)
		}
		before = ""
	}
	if err := h.sc.InitProject(ctx, jp); err != nil {
		return "", errHubIdentity("scion init: %w", err)
	}
	id, slug, err := readProjectMarker(ctx, h.jr, jp)
	if err != nil {
		return "", errHubIdentity("%w", err)
	}
	if prev != "" && id != prev && h.pinnedTo(ctx, jp, prev) {
		id = prev
	}
	if before != "" && id != before || prev != "" && id != prev {
		return "", errHubIdentity("the tree's project marker changed from project %s to %s during `scion init`; refusing", before, id)
	}
	if _, err := h.assertController(ctx, state.HubModeFresh); err != nil {
		return "", err
	}
	p, _, err := h.ctl.RegisterProject(ctx, id, slug, jp)
	if err != nil {
		return "", errHubIdentity("registering project %s: %w", slug, err)
	}
	return p.ID, nil
}

// checkProjectMarker returns the project ID the tree's marker names ("" when
// there is no readable marker and no prev). When prev is known the marker must
// be readable and resolve to it: an agent that could make it unreadable here
// could write its own once scion init has run.
//
// After register-project's re-init the marker names a new local ID, and the
// jail user's scion config for it pins hub.projectId to prev, as treeLinkedTo
// accepts. An agent choosing the marker can steer which config scion reads,
// but the answer must still be prev, so the project lever registers is the
// one it recorded.
func (h *hubIdentity) checkProjectMarker(ctx context.Context, jp, prev string) (string, error) {
	id, _, err := readProjectMarker(ctx, h.jr, jp)
	switch {
	case prev == "" && err != nil:
		return "", nil
	case err != nil:
		return "", errHubIdentity("lever last registered project %s, but the tree's project marker cannot be read (%v); an agent may have replaced it. %s",
			prev, err, markerFix(jp, prev))
	case prev != "" && id != prev:
		if h.pinnedTo(ctx, jp, prev) {
			return prev, nil
		}
		return "", errHubIdentity("the tree's project marker %s/.scion names project %s, but lever last registered %s; an agent may have rewritten it. %s",
			jp, id, prev, markerFix(jp, prev))
	}
	return id, nil
}

// pinnedTo reports whether the jail's scion resolves the tree's hub project to
// want through hub.projectId. A failed read is not a match.
func (h *hubIdentity) pinnedTo(ctx context.Context, jp, want string) bool {
	if h.sc == nil || want == "" {
		return false
	}
	id, err := h.sc.ConfigGetProject(ctx, jp, "hub.projectId")
	return err == nil && id == want
}

func markerFix(jp, prev string) string {
	return fmt.Sprintf("Restore %s/.scion naming project-id %s, or, to register the tree as a new project, "+
		"remove .lever-state/hub-identity.json and run `lever apply --reset-hub-identity`", jp, prev)
}

// readProjectMarker reads the project ID and slug scion init wrote into the
// tree's marker file. lever trees are never git repositories
// (config.checkNonGitTree), so the marker is the one-file form.
func readProjectMarker(ctx context.Context, jr proc.Runner, jp string) (id, slug string, err error) {
	res, err := jr.Run(proc.WithOutputLimit(ctx, smallGuestOutputLimit), nil, "sh", "-c", `[ -f "$1" ] && [ ! -L "$1" ] && cat "$1"`, "_", path.Join(jp, ".scion"))
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
	return controllerLifecycle(ctx, h.scopeKnown(), rec)
}

func (h *hubIdentity) controllerInbox(ctx context.Context) bool {
	rec, _, _ := h.st.LoadControllerPATRecord()
	return controllerInbox(ctx, h.scopeKnown(), rec)
}

func (h *hubIdentity) scopeKnown() scopeKnownFunc {
	if h.sc == nil {
		return nil
	}
	return h.sc.KnowsUATScope
}

// mintPATs mints the controller PAT (and the remote proxy's read PAT when
// remote access is on) as the controller, when patMintReason says so: save
// the token, then its record, then revoke the one it replaced, so a failure
// at any point leaves a working token on disk. The broker reads the PAT file
// per use, so this runs while agents run.
//
// Only a mint or a revoke asserts the controller (assertController), so on a
// migrated hub nothing else in the apply depends on the controller's address.
func (h *hubIdentity) mintPATs(ctx context.Context, mode, projectID string, ctlScopes []string) error {
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
		if _, err := h.assertController(ctx, mode); err != nil {
			// A token in its renew window still works: a contested address then
			// costs a warning, not the rest of the apply.
			if errors.Is(err, errControllerContested) && !patUrgent(tok, old, found, scopes, now) {
				h.warn("%s token not renewed this time (it stays valid until %s): %v", name, expiryText(old.ExpiresAt), err)
				return nil
			}
			return err
		}
		secret, t, err := h.ctl.CreateToken(ctx, name, projectID, scopes, now.Add(patLifetime))
		if err != nil {
			return errHubIdentity("minting %s: %w", name, err)
		}
		if miss := ungrantedScopes(scopes, t.Scopes); len(miss) > 0 {
			if err := h.ctl.RevokeToken(ctx, t.ID); err != nil && !errors.Is(err, hubapi.ErrNotFound) {
				h.warn("hub-identity: could not revoke the unusable %s token %s: %v", name, t.ID, err)
			}
			return errHubIdentity("the hub minted %s without %s, which the controller needs to start agents and read its inbox; "+
				"it checks each scope against the controller's role bindings on the project, so check the %s role and the project ownership",
				name, strings.Join(miss, ","), controllerRoleName)
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
	if err := mint("lever-controller", ctlScopes, h.st.LoadControllerPAT, h.st.LoadControllerPATRecord,
		h.st.SaveControllerPAT, h.st.SaveControllerPATRecord); err != nil {
		return err
	}
	if !h.app.RemoteEnabled() {
		// No proxy runs to use it, so the token is revoked rather than kept.
		rec, found, err := h.st.LoadRemotePATRecord()
		if err != nil {
			return errHubIdentity("%w", err)
		}
		if found && rec.ID != "" {
			// A failed revoke keeps the token and its record, so the next apply
			// tries again; with remote access off no proxy holds it.
			if _, err := h.assertController(ctx, mode); err != nil {
				h.warn("hub-identity: the remote proxy's read token %s stays valid until the next apply revokes it: %v", rec.ID, err)
				return nil
			}
			if err := h.ctl.RevokeToken(ctx, rec.ID); err != nil && !errors.Is(err, hubapi.ErrNotFound) {
				h.warn("hub-identity: the remote proxy's read token %s stays valid until the next apply revokes it: revoking it failed: %v", rec.ID, err)
				return nil
			}
		}
		if err := h.st.RemoveRemotePAT(); err != nil {
			return errHubIdentity("%w", err)
		}
		return nil
	}
	return mint("lever-remote", remotePATScopes(), h.st.LoadRemotePAT, h.st.LoadRemotePATRecord,
		h.st.SaveRemotePAT, h.st.SaveRemotePATRecord)
}

// patLifetime is just under scion's one-year maximum, clear of its
// "no later than now+1y" check on either clock.
const patLifetime = 360 * 24 * time.Hour

// controllerActions are the per-agent capability actions the hub reports for
// what lever does with the controller PAT: start, stop, suspend and resume
// are "lifecycle" on a hub that has it and "attach" on one that does not.
// Messaging is a scope action the agent list does not report; it is not
// probed here.
func controllerActions(lifecycle bool) []string {
	a := []string{"read", "attach", "delete"}
	if lifecycle {
		a = append(a, "lifecycle")
	}
	return a
}

// ungrantedScopes returns the requested scopes a mint must come back with
// and did not: the skill, template and harness_config reads and inbox:read.
// No alias expands to them, and scion drops or refuses a scope its issuer is
// not eligible for, so a token without them could start no baseline agent or
// read no notification. The other requested scopes are aliases the hub
// expands, so they are not compared.
func ungrantedScopes(requested, granted []string) []string {
	var out []string
	for _, s := range requested {
		literal := s == inboxReadScope || strings.HasPrefix(s, "skill:") || strings.HasPrefix(s, "template:") || strings.HasPrefix(s, "harness_config:")
		if literal && !slices.Contains(granted, s) {
			out = append(out, s)
		}
	}
	return out
}

// errAuthorityMissing marks a probe that read the hub's answer and found an
// authority missing, as opposed to one that could not read it.
var errAuthorityMissing = errors.New("missing authority")

// probeControllerAuthority checks, without changing anything, that the
// controller PAT still does everything lever does with it: the hub's own
// capability read for every agent action, plus the project reads and writes'
// read halves, the CLI list, the notification list when the token carries
// inbox:read, and the scopes rec says the hub granted (ungrantedScopes).
func probeControllerAuthority(ctx context.Context, pat *hubapi.Client, sc identityScion, projectID, jp string, lifecycle bool, rec state.PATRecord) error {
	var missing, failed []string
	// A record without a granted set (an older lever's mint) proves nothing
	// either way; the live reads below still run.
	if len(rec.Granted) > 0 {
		for _, s := range ungrantedScopes(rec.Requested, rec.Granted) {
			missing = append(missing, "scope "+s+" was not granted at mint")
		}
	}
	if slices.Contains(rec.Requested, inboxReadScope) {
		if err := pat.ReadNotifications(ctx); err != nil {
			if forbidden(err) {
				missing = append(missing, "notifications: "+err.Error())
			} else {
				failed = append(failed, "notifications: "+err.Error())
			}
		}
	}
	note := func(what string, err error) {
		if forbidden(err) {
			missing = append(missing, what+": "+err.Error())
		} else {
			failed = append(failed, what+": "+err.Error())
		}
	}
	if _, err := pat.Me(ctx); err != nil {
		note("auth/me", err)
	}
	if _, ok, err := pat.GetProject(ctx, projectID); err != nil {
		note("project "+projectID, err)
	} else if !ok {
		missing = append(missing, "project "+projectID+" is not visible to the controller")
	}
	if _, err := pat.SharedDirs(ctx, projectID); err != nil {
		note("shared dirs", err)
	}
	caps, err := pat.ProjectAgentCapabilities(ctx, projectID)
	if err != nil {
		note("agent capabilities", err)
	} else {
		if !slices.Contains(caps.Scope, "create") {
			missing = append(missing, fmt.Sprintf("agent create (scope actions %v)", caps.Scope))
		}
		for slug, acts := range caps.Agents {
			for _, want := range controllerActions(lifecycle) {
				if !slices.Contains(acts, want) {
					missing = append(missing, fmt.Sprintf("agent %s: %s (actions %v)", slug, want, acts))
				}
			}
		}
	}
	if _, err := sc.List(ctx, jp); err != nil {
		failed = append(failed, "scion list: "+err.Error())
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		return fmt.Errorf("%w: controller PAT probe: %s", errAuthorityMissing, strings.Join(missing, "; "))
	}
	if len(failed) > 0 {
		return fmt.Errorf("controller PAT probe could not complete: %s", strings.Join(failed, "; "))
	}
	return nil
}

// forbidden reports a hub answer that refused the credential's authority.
func forbidden(err error) bool {
	var ae *hubapi.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden
}

// revokeSSHTokens revokes, best effort, the tunnel tokens `lever ssh` minted
// and did not revoke itself (a killed process), once conduit is off: each
// carries agent:attach and agent:port_access, which reach agents through
// routes other than conduit too. A failure leaves the record for the next
// apply.
func (h *hubIdentity) revokeSSHTokens(ctx context.Context, mode string) {
	ids, err := h.st.SSHTokens()
	if err != nil || len(ids) == 0 {
		if err != nil {
			h.warn("hub-identity: %v", err)
		}
		return
	}
	if _, err := h.assertController(ctx, mode); err != nil {
		h.warn("hub-identity: the `lever ssh` tokens %s stay valid until they expire or the next apply revokes them: %v", strings.Join(ids, ", "), err)
		return
	}
	for _, id := range ids {
		if err := h.ctl.RevokeToken(ctx, id); err != nil && !errors.Is(err, hubapi.ErrNotFound) {
			h.warn("hub-identity: the `lever ssh` token %s stays valid until it expires or the next apply revokes it: %v", id, err)
			continue
		}
		if err := h.st.ForgetSSHToken(id); err != nil {
			h.warn("hub-identity: %v", err)
		}
	}
}
