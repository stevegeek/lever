package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// fakeAdminHub is an in-memory stand-in for the throwaway hub's admin API:
// the project list, role definitions, the user directory and role bindings,
// answering with scion's shapes (see internal/hubapi/roles.go).
type fakeAdminHub struct {
	projectID string
	roles     []hubapi.RoleDefinition
	// systemRoles are listed with roles but are scion's seeded ones; kept
	// apart so tests can count the roles lever created.
	systemRoles []hubapi.RoleDefinition
	users       []hubapi.User
	bindings    []hubapi.RoleBinding
	constraints []*fakeConstraint
	// previews maps an issued preview token to what it was issued for; a
	// commit consumes it, as scion's single-use tokens are.
	previews map[string]fakePreview
	// blockPreview makes every preview answer with commitBlocked.
	blockPreview bool
	// failCreateConstraint makes every constraint create answer 500.
	failCreateConstraint bool
	calls        []string // "METHOD path", in order
	next         int
}

type fakeConstraint struct {
	hubapi.ConstraintDraft
	id       string
	revision int
}

type fakePreview struct {
	op    string
	draft string // canonical JSON of the create draft
	id    string // the constraint a delete targets
}

// fakeHubMemberPermissions is scion's seeded hub-member set
// (pkg/hub/seed.go hubMemberPermissionIDs), project.create included.
var fakeHubMemberPermissions = []string{"user.read", "user.list", "group.read", "group.list",
	"template.read", "template.list", "hub.settings.read", "role.read", "project.create"}

func newFakeAdminHub(users ...string) *fakeAdminHub {
	h := &fakeAdminHub{projectID: "proj-uuid", previews: map[string]fakePreview{},
		systemRoles: []hubapi.RoleDefinition{{ID: "sys-hub-member", Name: "hub-member", ScopeType: "system",
			System: true, Permissions: fakeHubMemberPermissions}}}
	for i, e := range users {
		h.users = append(h.users, hubapi.User{ID: fmt.Sprintf("user-%d", i+1), Email: e})
	}
	return h
}

func (h *fakeAdminHub) id(kind string) string {
	h.next++
	return fmt.Sprintf("%s-%d", kind, h.next)
}

func (h *fakeAdminHub) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return h.DoBody(ctx, method, path, nil)
}

func (h *fakeAdminHub) DoBody(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	h.calls = append(h.calls, method+" "+path)
	u, err := url.Parse(path)
	if err != nil {
		return 0, nil, err
	}
	reply := func(status int, v any) (int, []byte, error) {
		b, _ := json.Marshal(v)
		return status, b, nil
	}
	switch {
	case method == http.MethodGet && u.Path == "/api/v1/projects":
		return reply(200, map[string]any{"projects": []map[string]string{{"id": h.projectID, "name": "lever", "slug": "lever"}}})
	case method == http.MethodGet && u.Path == "/api/v1/admin/roles":
		all := append(slices.Clone(h.systemRoles), h.roles...)
		return reply(200, map[string]any{"items": all, "totalCount": len(all)})
	case method == http.MethodPost && u.Path == "/api/v1/admin/roles":
		var rd hubapi.RoleDefinition
		if err := json.Unmarshal(body, &rd); err != nil {
			return reply(400, map[string]string{"error": err.Error()})
		}
		rd.ID = h.id("role")
		h.roles = append(h.roles, rd)
		return reply(201, rd)
	case method == http.MethodPut && strings.HasPrefix(u.Path, "/api/v1/admin/roles/"):
		id := strings.TrimPrefix(u.Path, "/api/v1/admin/roles/")
		for i := range h.roles {
			if h.roles[i].ID == id {
				var rd hubapi.RoleDefinition
				if err := json.Unmarshal(body, &rd); err != nil {
					return reply(400, map[string]string{"error": err.Error()})
				}
				h.roles[i].Name, h.roles[i].Description, h.roles[i].Permissions = rd.Name, rd.Description, rd.Permissions
				return reply(200, h.roles[i])
			}
		}
		return reply(404, map[string]string{"error": "not found"})
	case method == http.MethodGet && u.Path == "/api/v1/users":
		q := strings.ToLower(u.Query().Get("search"))
		var out []hubapi.User
		for _, usr := range h.users {
			if strings.Contains(strings.ToLower(usr.Email), q) {
				out = append(out, usr)
			}
		}
		return reply(200, map[string]any{"users": out, "totalCount": len(out)})
	case method == http.MethodGet && u.Path == "/api/v1/admin/role-bindings":
		var out []hubapi.RoleBinding
		for _, b := range h.bindings {
			if b.PrincipalType == u.Query().Get("principalType") && b.PrincipalID == u.Query().Get("principalId") {
				out = append(out, b)
			}
		}
		return reply(200, map[string]any{"items": out, "totalCount": len(out)})
	case method == http.MethodPost && u.Path == "/api/v1/admin/role-bindings":
		var rb hubapi.RoleBinding
		if err := json.Unmarshal(body, &rb); err != nil {
			return reply(400, map[string]string{"error": err.Error()})
		}
		for _, b := range h.bindings {
			if b.RoleDefinitionID == rb.RoleDefinitionID && b.PrincipalID == rb.PrincipalID && b.ScopeID == rb.ScopeID {
				return reply(409, map[string]string{"error": "this role binding already exists"})
			}
		}
		rb.ID = h.id("binding")
		h.bindings = append(h.bindings, rb)
		return reply(201, rb)
	}
	if status, v, ok := h.constraintRoute(method, u, body); ok {
		return reply(status, v)
	}
	return reply(404, map[string]string{"error": "no route " + method + " " + path})
}

// constraintRoute answers scion's access-constraint admin routes, with its
// preview-bound commits: a create or delete needs an unused token issued for
// exactly that draft or that constraint.
func (h *fakeAdminHub) constraintRoute(method string, u *url.URL, body []byte) (int, any, bool) {
	const base = "/api/v1/admin/access-constraints"
	wire := func(c *fakeConstraint, detail bool) map[string]any {
		m := map[string]any{"id": c.id, "name": c.Name, "subject": c.Subject,
			"scope": map[string]string{"type": c.Scope.Type}, "status": "active",
			"revision": fmt.Sprint(c.revision)}
		if detail {
			var perms []map[string]string
			for _, p := range c.MaximumPermissions {
				perms = append(perms, map[string]string{"id": p, "displayName": p})
			}
			m["maximumPermissions"] = perms
		}
		return m
	}
	find := func(id string) int {
		for i, c := range h.constraints {
			if c.id == id {
				return i
			}
		}
		return -1
	}
	consume := func(tok string) (fakePreview, bool) {
		p, ok := h.previews[tok]
		delete(h.previews, tok)
		return p, ok
	}
	switch {
	case method == http.MethodPost && u.Path == "/api/v1/admin/access-constraint-previews":
		var req struct {
			Operation    string                  `json:"operation"`
			Draft        *hubapi.ConstraintDraft `json:"draft"`
			ConstraintID string                  `json:"constraintId"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return 400, map[string]string{"error": err.Error()}, true
		}
		if h.blockPreview {
			return 200, map[string]any{"previewToken": "tok-blocked",
				"commitBlocked": map[string]string{"code": "preview_incomplete", "message": "preview is incomplete"}}, true
		}
		tok := h.id("tok")
		p := fakePreview{op: req.Operation, id: req.ConstraintID}
		if req.Draft != nil {
			b, _ := json.Marshal(req.Draft)
			p.draft = string(b)
		}
		h.previews[tok] = p
		return 200, map[string]any{"previewToken": tok}, true
	case method == http.MethodGet && u.Path == base:
		var items []map[string]any
		for _, c := range h.constraints {
			if strings.Contains(c.Name, u.Query().Get("nameContains")) {
				items = append(items, wire(c, false))
			}
		}
		return 200, map[string]any{"items": items, "totalCount": len(items)}, true
	case method == http.MethodPost && u.Path == base && h.failCreateConstraint:
		return 500, map[string]string{"error": "scripted create failure"}, true
	case method == http.MethodPost && u.Path == base:
		var req struct {
			hubapi.ConstraintDraft
			PreviewToken string `json:"previewToken"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return 400, map[string]string{"error": err.Error()}, true
		}
		b, _ := json.Marshal(req.ConstraintDraft)
		if p, ok := consume(req.PreviewToken); !ok || p.op != "create" || p.draft != string(b) {
			return 409, map[string]string{"error": "preview token invalid for this draft"}, true
		}
		for _, c := range h.constraints {
			if c.Name == req.Name && c.Scope.Type == req.Scope.Type {
				return 409, map[string]string{"error": "name already used at this scope"}, true
			}
		}
		c := &fakeConstraint{ConstraintDraft: req.ConstraintDraft, id: h.id("ac"), revision: 1}
		h.constraints = append(h.constraints, c)
		return 201, wire(c, true), true
	case strings.HasPrefix(u.Path, base+"/"):
		id := strings.TrimPrefix(u.Path, base+"/")
		i := find(id)
		if i < 0 {
			return 404, map[string]string{"error": "not found"}, true
		}
		switch method {
		case http.MethodGet:
			return 200, wire(h.constraints[i], true), true
		case http.MethodDelete:
			var req struct {
				PreviewToken string `json:"previewToken"`
			}
			_ = json.Unmarshal(body, &req)
			if p, ok := consume(req.PreviewToken); !ok || p.op != "delete" || p.id != id {
				return 409, map[string]string{"error": "preview token invalid for this delete"}, true
			}
			h.constraints = slices.Delete(h.constraints, i, i+1)
			return 200, map[string]string{"auditId": "audit-1"}, true
		}
	}
	return 0, nil, false
}

func (h *fakeAdminHub) count(prefix string) int {
	n := 0
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// seedAllPATs persists both tokens with current records, so a window opens
// only for the role grant.
func seedAllPATs(t *testing.T, st state.State) {
	t.Helper()
	seedPAT(t, st, "controller", "pat-controller")
	seedPAT(t, st, "remote", "pat-remote")
}

func collectWarnings() (*[]string, func(string, ...any)) {
	var w []string
	return &w, func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }
}

// The role is the remote PAT's scopes in registry form, plus project.list,
// which only the SPA's project list needs.
func TestRemoteRolePermissionsMirrorTheRemotePAT(t *testing.T) {
	want := []string{"agent.read", "agent.list", "project.read", "agent.attach", "agent.message", "project.list"}
	if got := remoteRolePermissions(); !slices.Equal(got, want) {
		t.Fatalf("remoteRolePermissions = %v, want %v", got, want)
	}
}

// First grant: the PATs exist, so the window opens for the role alone. It
// creates the project role with exactly the five permissions, binds the
// allowed user on the instance project, and records it.
func TestRemoteWebRoleFirstGrant(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github")
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	warned, warn := collectWarnings()

	ra := remoteAccess{Enabled: true, Emails: []string{"you@github"}}
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionServerStart) }); n != 1 {
		t.Fatalf("server start calls = %d, want 1", n)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionTokenCreate) }); n != 0 {
		t.Fatalf("token create calls = %d, want 0 (both PATs current)", n)
	}
	if len(hub.roles) != 1 {
		t.Fatalf("roles = %+v, want one", hub.roles)
	}
	r := hub.roles[0]
	if r.Name != remoteWebRoleName || r.ScopeType != "project" || !slices.Equal(r.Permissions, remoteRolePermissions()) {
		t.Fatalf("role = %+v", r)
	}
	want := hubapi.RoleBinding{ID: "binding-2", RoleDefinitionID: r.ID, PrincipalType: "user", PrincipalID: "user-1", ScopeType: "project", ScopeID: "proj-uuid"}
	if len(hub.bindings) != 1 || hub.bindings[0] != want {
		t.Fatalf("bindings = %+v, want [%+v]", hub.bindings, want)
	}
	rec, found, err := st.LoadRemoteRoleRecord()
	if err != nil || !found {
		t.Fatalf("record: found=%v err=%v", found, err)
	}
	if rec.RoleID != r.ID || rec.ProjectID != "proj-uuid" || rec.Bound["you@github"] != "user-1" || len(rec.Pending) != 0 {
		t.Fatalf("record = %+v", rec)
	}
	// The ceiling: one system-scope constraint on exactly that user, holding
	// hub-member minus project.create plus the role's permissions.
	wantCeiling := remoteCeilingPermissions(fakeHubMemberPermissions, remoteRolePermissions())
	if len(hub.constraints) != 1 {
		t.Fatalf("constraints = %+v, want one", hub.constraints)
	}
	c := hub.constraints[0]
	if c.Name != remoteCeilingName("you@github") || c.Scope.Type != "system" ||
		c.Subject != (hubapi.ConstraintSubject{Kind: "principal", PrincipalType: "user", PrincipalID: "user-1"}) ||
		!hubapi.SamePermissions(c.MaximumPermissions, wantCeiling) {
		t.Fatalf("constraint = %+v", c)
	}
	if slices.Contains(c.MaximumPermissions, "project.create") {
		t.Fatalf("ceiling keeps project.create: %v", c.MaximumPermissions)
	}
	for _, p := range append(slices.Clone(remoteRolePermissions()), "user.read", "hub.settings.read") {
		if !slices.Contains(c.MaximumPermissions, p) {
			t.Fatalf("ceiling drops %s, which the web user holds: %v", p, c.MaximumPermissions)
		}
	}
	if rec.Ceilings["you@github"] != c.id || !hubapi.SamePermissions(rec.CeilingPermissions, wantCeiling) {
		t.Fatalf("record ceiling = %v %v, want %s %v", rec.Ceilings, rec.CeilingPermissions, c.id, wantCeiling)
	}
	if fi, err := os.Stat(st.RemoteRole()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("remote-role.json: %v, %v", fi, err)
	}
	if len(*warned) != 0 {
		t.Fatalf("warnings = %q, want none", *warned)
	}
}

// A complete record means no window at all: no scion call, no hub call.
func TestRemoteWebRoleCompleteRecordOpensNoWindow(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github")
	ra := remoteAccess{Enabled: true, Emails: []string{"you@github"}}

	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	calls := len(hub.calls)

	again := proc.NewFakeRunner() // no scripts: any call is an error
	if err := ensureControllerPAT(context.Background(), again, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	if len(again.Calls) != 0 || len(hub.calls) != calls {
		t.Fatalf("re-run made %d jail calls and %d hub calls, want none", len(again.Calls), len(hub.calls)-calls)
	}
}

// A user who has never signed in has no hub user to bind. That is not an
// error: it is warned once with the fix, recorded as pending, and the
// record stays incomplete so the next apply retries.
func TestRemoteWebRoleMissingUserWarnsAndStaysPending(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github") // the second allowed user has never signed in
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	warned, warn := collectWarnings()

	ra := remoteAccess{Enabled: true, Emails: []string{"you@github", "partner@github"}}
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatalf("a missing user must not fail apply: %v", err)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "partner@github") || !strings.Contains((*warned)[0], remoteRoleFix) {
		t.Fatalf("warnings = %q, want one naming partner@github with the fix", *warned)
	}
	rec, found, _ := st.LoadRemoteRoleRecord()
	if !found || !slices.Equal(rec.Pending, []string{"partner@github"}) || rec.Bound["you@github"] == "" {
		t.Fatalf("record = %+v", rec)
	}
	if reason := remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()); !strings.Contains(reason, "partner@github") {
		t.Fatalf("reason = %q, want the record incomplete for partner@github", reason)
	}

	// The partner signs in; the next apply binds them and does not bind the
	// first user twice.
	hub.users = append(hub.users, hubapi.User{ID: "user-2", Email: "partner@github"})
	f2 := proc.NewFakeRunner()
	scriptPATMintChain(f2)
	if err := ensureControllerPAT(context.Background(), f2, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatal(err)
	}
	if len(hub.bindings) != 2 || hub.count("POST /api/v1/admin/role-bindings") != 2 {
		t.Fatalf("bindings = %+v (posts %d), want one per user", hub.bindings, hub.count("POST /api/v1/admin/role-bindings"))
	}
	rec, found, _ = st.LoadRemoteRoleRecord()
	if reason := remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()); reason != "" {
		t.Fatalf("reason = %q, want complete", reason)
	}
}

// Adding an allowed user re-triggers the window; the role is reused, not
// recreated, and the existing binding is not posted again.
func TestRemoteWebRoleAllowedUsersChangeRetriggers(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github", "partner@github")
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever",
		remoteAccess{Enabled: true, Emails: []string{"you@github"}}, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}

	f2 := proc.NewFakeRunner()
	scriptPATMintChain(f2)
	ra := remoteAccess{Enabled: true, Emails: []string{"you@github", "partner@github"}}
	if err := ensureControllerPAT(context.Background(), f2, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(f2.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionServerStart) }); n != 1 {
		t.Fatalf("server start calls = %d, want 1 (the new user re-opens the window)", n)
	}
	if len(hub.roles) != 1 || hub.count("POST /api/v1/admin/roles") != 1 {
		t.Fatalf("roles = %+v, want the first one reused", hub.roles)
	}
	if len(hub.bindings) != 2 || hub.count("POST /api/v1/admin/role-bindings") != 2 {
		t.Fatalf("bindings = %+v, want one per user and no duplicate post", hub.bindings)
	}
	rec, found, _ := st.LoadRemoteRoleRecord()
	if reason := remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()); reason != "" {
		t.Fatalf("reason = %q, want complete", reason)
	}
}

// A role that exists with a different permission set (edited by hand, or
// granted by an older lever) is rewritten to the current set, with a warning.
// A record written with an older permission set re-triggers the window too.
func TestRemoteWebRolePermissionDriftConverges(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	if err := st.SaveRemoteRoleRecord(state.RemoteRoleRecord{
		RoleID: "role-old", Permissions: []string{"agent.read"}, Bound: map[string]string{"you@github": "user-1"},
	}); err != nil {
		t.Fatal(err)
	}
	hub := newFakeAdminHub("you@github")
	hub.roles = []hubapi.RoleDefinition{{ID: "role-old", Name: remoteWebRoleName, ScopeType: "project", Permissions: []string{"agent.read", "agent.manage"}}}
	hub.bindings = []hubapi.RoleBinding{{ID: "b-1", RoleDefinitionID: "role-old", PrincipalType: "user", PrincipalID: "user-1", ScopeType: "project", ScopeID: "proj-uuid"}}
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	warned, warn := collectWarnings()

	ra := remoteAccess{Enabled: true, Emails: []string{"you@github"}}
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatal(err)
	}
	if hub.count("PUT /api/v1/admin/roles/role-old") != 1 || hub.count("POST /api/v1/admin/roles") != 0 {
		t.Fatalf("calls = %v, want one PUT of role-old and no create", hub.calls)
	}
	if !hubapi.SamePermissions(hub.roles[0].Permissions, remoteRolePermissions()) {
		t.Fatalf("role permissions = %v, want %v", hub.roles[0].Permissions, remoteRolePermissions())
	}
	if hub.count("POST /api/v1/admin/role-bindings") != 0 {
		t.Fatalf("the existing binding was posted again: %v", hub.calls)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "agent.manage") {
		t.Fatalf("warnings = %q, want one naming the drifted set", *warned)
	}
	rec, found, _ := st.LoadRemoteRoleRecord()
	if reason := remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()); reason != "" {
		t.Fatalf("reason = %q, want complete", reason)
	}
}

// A hub failure mid-grant is a warning, not an apply error, and leaves no
// record, so the next apply retries.
func TestRemoteWebRoleHubFailureIsAWarning(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	warned, warn := collectWarnings()
	bad := failingDoer{} // every hub call answers 403
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever",
		remoteAccess{Enabled: true, Emails: []string{"you@github"}}, patMintOpts{AdminHub: bad, Warn: warn}); err != nil {
		t.Fatalf("a failed grant must not fail apply: %v", err)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "remote web role not granted") {
		t.Fatalf("warnings = %q", *warned)
	}
	if _, found, _ := st.LoadRemoteRoleRecord(); found {
		t.Fatal("a failed grant must not leave a record")
	}
}

// A record written before the ceiling existed re-opens the window. The role
// and binding are reused; only the ceiling is added.
func TestRemoteCeilingPreCeilingRecordRetriggers(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github")
	hub.roles = []hubapi.RoleDefinition{{ID: "role-1", Name: remoteWebRoleName, ScopeType: "project", Permissions: remoteRolePermissions()}}
	hub.bindings = []hubapi.RoleBinding{{ID: "b-1", RoleDefinitionID: "role-1", PrincipalType: "user", PrincipalID: "user-1", ScopeType: "project", ScopeID: "proj-uuid"}}
	// Exactly what 2b160af wrote: no ceilings, no ceiling_permissions.
	if err := os.WriteFile(st.RemoteRole(), []byte(`{"role_id":"role-1","permissions":["agent.read","agent.list","project.read","agent.attach","agent.message","project.list"],"project_id":"proj-uuid","bound":{"you@github":"user-1"},"granted_at":"2026-09-23T20:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ra := remoteAccess{Enabled: true, Emails: []string{"you@github"}}
	rec, found, err := st.LoadRemoteRoleRecord()
	if err != nil || !found {
		t.Fatalf("old record: found=%v err=%v", found, err)
	}
	if reason := remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()); !strings.Contains(reason, "ceiling") {
		t.Fatalf("reason = %q, want the missing ceiling", reason)
	}

	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", ra, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvScionServerStart) }); n != 1 {
		t.Fatalf("server start calls = %d, want 1", n)
	}
	if hub.count("POST /api/v1/admin/roles") != 0 || hub.count("POST /api/v1/admin/role-bindings") != 0 {
		t.Fatalf("the role or binding was written again: %v", hub.calls)
	}
	if len(hub.constraints) != 1 || hub.count("POST /api/v1/admin/access-constraints") != 1 {
		t.Fatalf("constraints = %+v, want one created", hub.constraints)
	}
	rec, found, _ = st.LoadRemoteRoleRecord()
	if reason := remoteRoleReason(rec, found, ra.Emails, remoteRolePermissions()); reason != "" {
		t.Fatalf("reason = %q, want complete", reason)
	}
}

// Idempotent: a matching ceiling is read and kept, never re-created, and no
// preview is asked for.
func TestRemoteCeilingIdempotent(t *testing.T) {
	hub := newFakeAdminHub("you@github")
	hc := &hubapi.Client{T: hub}
	_, warn := collectWarnings()
	if _, err := ensureRemoteWebRole(context.Background(), hc, "lever", []string{"you@github"}, time.Now(), warn); err != nil {
		t.Fatal(err)
	}
	first := hub.constraints[0].id
	before := len(hub.calls)
	rec, err := ensureRemoteWebRole(context.Background(), hc, "lever", []string{"you@github"}, time.Now(), warn)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range hub.calls[before:] {
		if strings.HasPrefix(c, "POST /api/v1/admin/access-constraint") || strings.HasPrefix(c, "DELETE ") {
			t.Fatalf("second run wrote a constraint: %v", hub.calls[before:])
		}
	}
	if len(hub.constraints) != 1 || rec.Ceilings["you@github"] != first {
		t.Fatalf("constraints = %+v, record = %v; want the first one kept", hub.constraints, rec.Ceilings)
	}
}

// A ceiling that drifted (it allows project.create, or names a re-created hub
// user) is replaced through a preview-bound delete and create, with a warning.
func TestRemoteCeilingDriftIsReplaced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		perms   []string
	}{
		{"allows project.create", "user-1", append(slices.Clone(remoteCeilingPermissions(fakeHubMemberPermissions, remoteRolePermissions())), "project.create")},
		{"another hub user", "user-old", remoteCeilingPermissions(fakeHubMemberPermissions, remoteRolePermissions())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeAdminHub("you@github")
			hub.constraints = []*fakeConstraint{{id: "ac-old", revision: 3, ConstraintDraft: hubapi.ConstraintDraft{
				Name: remoteCeilingName("you@github"), Purpose: "x",
				Subject:            hubapi.ConstraintSubject{Kind: "principal", PrincipalType: "user", PrincipalID: tc.subject},
				Scope:              hubapi.ConstraintScope{Type: "system"},
				MaximumPermissions: tc.perms,
			}}}
			warned, warn := collectWarnings()
			rec, err := ensureRemoteWebRole(context.Background(), &hubapi.Client{T: hub}, "lever", []string{"you@github"}, time.Now(), warn)
			if err != nil {
				t.Fatal(err)
			}
			if hub.count("DELETE /api/v1/admin/access-constraints/ac-old") != 1 {
				t.Fatalf("calls = %v, want the old constraint deleted", hub.calls)
			}
			if len(hub.constraints) != 1 || hub.constraints[0].id == "ac-old" || hub.constraints[0].Subject.PrincipalID != "user-1" ||
				slices.Contains(hub.constraints[0].MaximumPermissions, "project.create") {
				t.Fatalf("constraints = %+v", hub.constraints)
			}
			if rec.Ceilings["you@github"] != hub.constraints[0].id {
				t.Fatalf("record ceilings = %v", rec.Ceilings)
			}
			if len(*warned) != 1 || !strings.Contains((*warned)[0], "ac-old") {
				t.Fatalf("warnings = %q", *warned)
			}
		})
	}
}

// A preview the hub blocks fails the grant: no record, so the next apply
// retries, and apply itself carries on with a warning.
func TestRemoteCeilingBlockedPreviewLeavesNoRecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedAllPATs(t, st)
	hub := newFakeAdminHub("you@github")
	hub.blockPreview = true
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	warned, warn := collectWarnings()
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever",
		remoteAccess{Enabled: true, Emails: []string{"you@github"}}, patMintOpts{AdminHub: hub, Warn: warn}); err != nil {
		t.Fatalf("a blocked ceiling must not fail apply: %v", err)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "preview is incomplete") {
		t.Fatalf("warnings = %q", *warned)
	}
	if _, found, _ := st.LoadRemoteRoleRecord(); found {
		t.Fatal("a failed ceiling must not leave a record")
	}
}

// Without the hub-member role lever cannot compute a ceiling that keeps the
// user's reads, so it refuses rather than guess.
func TestRemoteCeilingNeedsTheHubMemberRole(t *testing.T) {
	hub := newFakeAdminHub("you@github")
	hub.systemRoles = nil
	_, warn := collectWarnings()
	_, err := ensureRemoteWebRole(context.Background(), &hubapi.Client{T: hub}, "lever", []string{"you@github"}, time.Now(), warn)
	if err == nil || !strings.Contains(err.Error(), "hub-member") {
		t.Fatalf("err = %v, want the missing hub-member role named", err)
	}
	if len(hub.constraints) != 0 || len(hub.bindings) != 0 {
		t.Fatalf("wrote %d constraints and %d bindings before failing", len(hub.constraints), len(hub.bindings))
	}
}

type failingDoer struct{}

func (failingDoer) Do(context.Context, string, string) (int, []byte, error) {
	return 403, []byte(`{"error":"forbidden"}`), nil
}

func (failingDoer) DoBody(ctx context.Context, m, p string, _ []byte) (int, []byte, error) {
	return failingDoer{}.Do(ctx, m, p)
}

// Remote disabled: the role record is never consulted and nothing opens.
func TestRemoteWebRoleDisabledDoesNothing(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := proc.NewFakeRunner()
	hub := newFakeAdminHub("you@github")
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{AdminHub: hub}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 || len(hub.calls) != 0 {
		t.Fatalf("calls: jail %d hub %d, want none", len(f.Calls), len(hub.calls))
	}
}

func TestCheckRemoteWebRole(t *testing.T) {
	perms := remoteRolePermissions()
	complete := state.RemoteRoleRecord{RoleID: "r", Permissions: perms, Bound: map[string]string{"you@github": "u1"},
		Ceilings: map[string]string{"you@github": "ac-1"}, CeilingPermissions: perms, GrantedAt: time.Now()}
	uncapped := complete
	uncapped.Ceilings, uncapped.CeilingPermissions = nil, nil
	loose := complete
	loose.CeilingPermissions = append(slices.Clone(perms), "project.create")
	for _, tc := range []struct {
		name    string
		enabled bool
		rec     *state.RemoteRoleRecord
		emails  []string
		ok      bool
		detail  string
		fix     string
	}{
		{"disabled", false, nil, nil, true, "disabled", ""},
		{"no record", true, nil, []string{"you@github"}, false, "no grant recorded", "lever apply"},
		{"complete", true, &complete, []string{"you@github"}, true, "you@github", ""},
		{"new user", true, &complete, []string{"you@github", "partner@github"}, false, "partner@github", "lever apply"},
		{"pending user", true, &state.RemoteRoleRecord{Permissions: perms, Bound: map[string]string{}, Pending: []string{"you@github"}}, []string{"you@github"}, false, "never signed in", "sign in once"},
		{"drift", true, &state.RemoteRoleRecord{Permissions: []string{"agent.read"}, Bound: complete.Bound}, []string{"you@github"}, false, "agent.read", "lever apply"},
		{"no ceiling (pre-ceiling record)", true, &uncapped, []string{"you@github"}, false, "can create projects: you@github", "lever apply"},
		{"ceiling allows project.create", true, &loose, []string{"you@github"}, false, "project-create ceiling was written with", "lever apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.ForConfig(t.TempDir())
			if tc.rec != nil {
				if err := st.SaveRemoteRoleRecord(*tc.rec); err != nil {
					t.Fatal(err)
				}
			}
			got := checkRemoteWebRole(st, remoteAccess{Enabled: tc.enabled, Emails: tc.emails})
			if got.ok != tc.ok || !strings.Contains(got.detail, tc.detail) || !strings.Contains(got.fix, tc.fix) {
				t.Fatalf("got %+v, want ok=%v detail~%q fix~%q", got, tc.ok, tc.detail, tc.fix)
			}
		})
	}
}
