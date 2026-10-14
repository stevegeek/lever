package host

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/hubapi"
)

// fakeAdminHub is an in-memory hub admin API answering with scion's
// shapes (see internal/hubapi/roles.go).
type fakeAdminHub struct {
	projectID string
	roles     []hubapi.RoleDefinition
	// systemRoles are scion's seeded roles, kept apart so tests can count the
	// roles lever created.
	systemRoles []hubapi.RoleDefinition
	users       []hubapi.User
	bindings    []hubapi.RoleBinding
	constraints []*fakeConstraint
	// previews maps an issued preview token to its subject; a commit consumes it,
	// as scion's single-use tokens are.
	previews             map[string]fakePreview
	blockPreview         bool
	failCreateConstraint bool
	// actorLacks are permissions the admin does not hold, as scion's
	// checkRelaxationAuthority sees them: a delete of a constraint naming one,
	// or an update adding one, is refused.
	actorLacks []string
	calls      []string // "METHOD path", in order
	next       int
	// me is the user id /api/v1/auth/me answers (the dev user); scopes is what
	// /api/v1/auth/scopes lists.
	me     string
	scopes []string
	// ifMatch is the current request's If-Match.
	ifMatch string
}

type fakeConstraint struct {
	hubapi.ConstraintDraft
	id       string
	revision int
	status   string // "" is active
}

type fakePreview struct {
	op    string
	draft string // canonical JSON of the create or update draft
	id    string // the constraint an update or delete targets
	rev   string // the revision an update or delete was previewed at
}

// fakeHubMemberPermissions is scion's seeded hub-member set, project.create
// included.
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

func (h *fakeAdminHub) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return h.DoBodyIfMatch(ctx, method, path, body, "")
}

func (h *fakeAdminHub) DoBodyIfMatch(_ context.Context, method, path string, body []byte, ifMatch string) (int, []byte, error) {
	h.calls = append(h.calls, method+" "+path)
	h.ifMatch = ifMatch
	u, err := url.Parse(path)
	if err != nil {
		return 0, nil, err
	}
	reply := func(status int, v any) (int, []byte, error) {
		b, _ := json.Marshal(v)
		return status, b, nil
	}
	switch {
	case method == http.MethodGet && u.Path == "/api/v1/auth/me":
		if h.me == "" {
			return reply(401, map[string]string{"error": "unauthenticated"})
		}
		return reply(200, map[string]string{"id": h.me, "email": "dev@localhost"})
	case method == http.MethodGet && u.Path == "/api/v1/auth/scopes":
		var out []map[string]string
		for _, sc := range h.scopes {
			out = append(out, map[string]string{"id": sc})
		}
		return reply(200, map[string]any{"scopes": out})
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
	case method == http.MethodDelete && strings.HasPrefix(u.Path, "/api/v1/admin/role-bindings/"):
		id := strings.TrimPrefix(u.Path, "/api/v1/admin/role-bindings/")
		for i, b := range h.bindings {
			if b.ID == id {
				h.bindings = slices.Delete(h.bindings, i, i+1)
				return reply(204, nil)
			}
		}
		return reply(404, map[string]string{"error": "not found"})
	}
	if status, v, ok := h.constraintRoute(method, u, body); ok {
		return reply(status, v)
	}
	return reply(404, map[string]string{"error": "no route " + method + " " + path})
}

// constraintRoute answers scion's access-constraint routes: a create or delete
// needs an unused preview token issued for exactly that draft or constraint.
func (h *fakeAdminHub) constraintRoute(method string, u *url.URL, body []byte) (int, any, bool) {
	const base = "/api/v1/admin/access-constraints"
	wire := func(c *fakeConstraint, detail bool) map[string]any {
		m := map[string]any{"id": c.id, "name": c.Name, "subject": c.Subject,
			"scope": map[string]string{"type": c.Scope.Type}, "status": cmp.Or(c.status, "active"),
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
			BaseRevision string                  `json:"baseRevision"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return 400, map[string]string{"error": err.Error()}, true
		}
		if h.blockPreview {
			return 200, map[string]any{"previewToken": "tok-blocked",
				"commitBlocked": map[string]string{"code": "preview_incomplete", "message": "preview is incomplete"}}, true
		}
		tok := h.id("tok")
		p := fakePreview{op: req.Operation, id: req.ConstraintID, rev: req.BaseRevision}
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
			c := h.constraints[i]
			if p, ok := consume(req.PreviewToken); !ok || p.op != "delete" || p.id != id || p.rev != fmt.Sprint(c.revision) {
				return 409, map[string]string{"error": "preview token invalid for this delete"}, true
			}
			if miss := h.lacking(c.MaximumPermissions); len(miss) > 0 {
				return relaxationRefusal(miss)
			}
			h.constraints = slices.Delete(h.constraints, i, i+1)
			return 200, map[string]string{"auditId": "audit-1"}, true
		case http.MethodPut:
			var req struct {
				hubapi.ConstraintDraft
				PreviewToken string `json:"previewToken"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				return 400, map[string]string{"error": err.Error()}, true
			}
			c := h.constraints[i]
			if h.ifMatch == "" {
				return 428, map[string]string{"error": "If-Match header is required for updates"}, true
			}
			b, _ := json.Marshal(req.ConstraintDraft)
			if p, ok := consume(req.PreviewToken); !ok || p.op != "update" || p.id != id || p.draft != string(b) || p.rev != h.ifMatch {
				return 409, map[string]string{"error": "preview token invalid for this update"}, true
			}
			if h.ifMatch != fmt.Sprint(c.revision) {
				return 409, map[string]string{"error": "stale revision"}, true
			}
			if req.Name != c.Name || req.Subject != c.Subject || req.Scope != c.Scope {
				return 400, map[string]string{"error": "the fake hub takes only permission changes"}, true
			}
			var added []string
			for _, p := range req.MaximumPermissions {
				if !slices.Contains(c.MaximumPermissions, p) {
					added = append(added, p)
				}
			}
			if miss := h.lacking(added); len(miss) > 0 {
				return relaxationRefusal(miss)
			}
			c.MaximumPermissions, c.Purpose, c.revision = req.MaximumPermissions, req.Purpose, c.revision+1
			return 200, wire(c, true), true
		}
	}
	return 0, nil, false
}

func (h *fakeAdminHub) lacking(perms []string) []string {
	var out []string
	for _, p := range perms {
		if slices.Contains(h.actorLacks, p) {
			out = append(out, p)
		}
	}
	return out
}

// relaxationRefusal is scion's answer from checkRelaxationAuthority.
func relaxationRefusal(missing []string) (int, any, bool) {
	return 403, map[string]any{"error": map[string]any{"code": "insufficient_constraint_relaxation_authority",
		"message": fmt.Sprintf("actor lacks authority over %d permission(s) being restored", len(missing)),
		"details": map[string]any{"missingPermissionIds": missing}}}, true
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

func collectWarnings() (*[]string, func(string, ...any)) {
	var w []string
	return &w, func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }
}

// The role is the remote PAT's scopes in registry form, plus project.list,
// which only the SPA's project list needs.
func TestRemoteRolePermissionsMirrorTheRemotePAT(t *testing.T) {
	want := []string{"agent.read", "agent.list", "project.read", "agent.attach", "agent.message", "project.list"}
	if got := remoteRolePermissions(false); !slices.Equal(got, want) {
		t.Fatalf("remoteRolePermissions = %v, want %v", got, want)
	}
}

// The ceiling is hub-member minus project.create plus the role, deduplicated
// and sorted.
func TestRemoteCeilingPermissions(t *testing.T) {
	got := remoteCeilingPermissions(fakeHubMemberPermissions, remoteRolePermissions(false))
	if slices.Contains(got, projectCreatePermission) {
		t.Fatalf("ceiling keeps project.create: %v", got)
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Fatalf("ceiling not sorted and unique: %v", got)
	}
	for _, p := range append(slices.Clone(remoteRolePermissions(false)), "user.read", "hub.settings.read") {
		if !slices.Contains(got, p) {
			t.Fatalf("ceiling drops %s: %v", p, got)
		}
	}
	if got := remoteCeilingPermissions(nil, []string{"project.create", "agent.message"}); !slices.Equal(got, []string{"agent.message"}) {
		t.Fatalf("project.create from the role side must be dropped too: %v", got)
	}
}

// A lever role is created with exactly its permissions, kept when it matches,
// and rewritten with a warning when it drifted; a system role of the same
// name is never touched.
func TestEnsureProjectRole(t *testing.T) {
	ctx := context.Background()
	hub := newFakeAdminHub()
	hc := &hubapi.Client{T: hub}
	warned, warn := collectWarnings()
	perms := remoteRolePermissions(false)
	defs := func() []hubapi.RoleDefinition {
		d, err := hc.RoleDefinitions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	r, err := ensureProjectRole(ctx, hc, defs(), remoteWebRoleName, remoteWebRoleDescription, perms, warn)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == "" || len(hub.roles) != 1 || hub.roles[0].ScopeType != hubapi.RoleScopeProject || !slices.Equal(hub.roles[0].Permissions, perms) {
		t.Fatalf("roles = %+v", hub.roles)
	}
	before := len(hub.calls)
	if r2, err := ensureProjectRole(ctx, hc, defs(), remoteWebRoleName, remoteWebRoleDescription, perms, warn); err != nil || r2.ID != r.ID {
		t.Fatalf("second run: %+v %v", r2, err)
	}
	for _, c := range hub.calls[before:] {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("a matching role was written: %v", hub.calls[before:])
		}
	}
	hub.roles[0].Permissions = append(slices.Clone(perms), "agent.manage")
	if _, err := ensureProjectRole(ctx, hc, defs(), remoteWebRoleName, remoteWebRoleDescription, perms, warn); err != nil {
		t.Fatal(err)
	}
	if hub.count("PUT /api/v1/admin/roles/"+r.ID) != 1 || !hubapi.SamePermissions(hub.roles[0].Permissions, perms) {
		t.Fatalf("drift not converged: %v %v", hub.calls, hub.roles[0].Permissions)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "agent.manage") {
		t.Fatalf("warnings = %q", *warned)
	}

	sys := newFakeAdminHub()
	sys.systemRoles = append(sys.systemRoles, hubapi.RoleDefinition{ID: "sys-x", Name: contactRoleName, ScopeType: hubapi.RoleScopeProject, System: true})
	sc := &hubapi.Client{T: sys}
	sdefs, err := sc.RoleDefinitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureProjectRole(ctx, sc, sdefs, contactRoleName, contactRoleDescription, contactRolePermissions(), warn); err == nil {
		t.Fatal("a system role of lever's name must be refused")
	}
	if sys.count("PUT ")+sys.count("POST ") != 0 {
		t.Fatalf("the system role was written: %v", sys.calls)
	}
}

// A binding is posted once; an existing one is not posted again, and drop
// removes only the named role's binding on the project.
func TestEnsureAndDropProjectRoleBinding(t *testing.T) {
	ctx := context.Background()
	hub := newFakeAdminHub()
	hc := &hubapi.Client{T: hub}
	for range 2 {
		if err := ensureProjectRoleBinding(ctx, hc, "role-a", "user-1", "proj-uuid"); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureProjectRoleBinding(ctx, hc, "role-b", "user-1", "proj-uuid"); err != nil {
		t.Fatal(err)
	}
	if hub.count("POST /api/v1/admin/role-bindings") != 2 || len(hub.bindings) != 2 {
		t.Fatalf("bindings = %+v, calls %v", hub.bindings, hub.calls)
	}
	if err := dropProjectRoleBinding(ctx, hc, "role-a", "user-1", "proj-uuid"); err != nil {
		t.Fatal(err)
	}
	if len(hub.bindings) != 1 || hub.bindings[0].RoleDefinitionID != "role-b" {
		t.Fatalf("after drop: %+v", hub.bindings)
	}
	before := len(hub.calls)
	if err := dropProjectRoleBinding(ctx, hc, "", "user-1", "proj-uuid"); err != nil || len(hub.calls) != before {
		t.Fatalf("drop of role \"\" must be a no-op: %v %v", err, hub.calls[before:])
	}
}

// ceilingFor is a system-scope ceiling on userID under name.
func ceilingFor(id, name, userID string, perms []string) *fakeConstraint {
	return &fakeConstraint{id: id, revision: 3, ConstraintDraft: hubapi.ConstraintDraft{
		Name: name, Purpose: "x",
		Subject:            hubapi.ConstraintSubject{Kind: "principal", PrincipalType: "user", PrincipalID: userID},
		Scope:              hubapi.ConstraintScope{Type: "system"},
		MaximumPermissions: perms,
	}}
}

func webCeiling() []string {
	return remoteCeilingPermissions(fakeHubMemberPermissions, remoteRolePermissions(false))
}

// callAt is the index of the first call with prefix, or -1.
func (h *fakeAdminHub) callAt(prefix string) int {
	return slices.IndexFunc(h.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// A first ceiling goes under the user's first name: system scope, on that
// user, hub-member minus project.create plus the role.
func TestRemoteCeilingFirstCreate(t *testing.T) {
	hub := newFakeAdminHub()
	warned, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(hub.constraints) != 1 {
		t.Fatalf("constraints = %+v, want one", hub.constraints)
	}
	c := hub.constraints[0]
	if c.id != id || c.Name != remoteCeilingName("you@github") || c.Scope.Type != "system" ||
		c.Subject != (hubapi.ConstraintSubject{Kind: "principal", PrincipalType: "user", PrincipalID: "user-1"}) ||
		!hubapi.SamePermissions(c.MaximumPermissions, webCeiling()) {
		t.Fatalf("constraint = %+v (id %q)", c, id)
	}
	if hub.count("DELETE ") != 0 || len(*warned) != 0 {
		t.Fatalf("a first create deleted or warned: %v %q", hub.calls, *warned)
	}
}

// A matching ceiling under either of the user's names is read and kept: no
// preview, create or delete.
func TestRemoteCeilingMatchingIsKept(t *testing.T) {
	for i, name := range remoteCeilingNames("you@github") {
		t.Run(name, func(t *testing.T) {
			hub := newFakeAdminHub()
			hub.constraints = []*fakeConstraint{ceilingFor("ac-keep", name, "user-1", webCeiling())}
			_, warn := collectWarnings()
			id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
			if err != nil {
				t.Fatalf("name %d: %v", i, err)
			}
			if id != "ac-keep" || len(hub.constraints) != 1 || hub.count("POST ")+hub.count("DELETE ") != 0 {
				t.Fatalf("id %q, constraints %+v, calls %v; want ac-keep kept untouched", id, hub.constraints, hub.calls)
			}
		})
	}
}

// The user's own ceiling with other permissions is rewritten in place: same
// constraint, same name, no create and no delete, and the hub's If-Match
// revision is the one read.
func TestRemoteCeilingMismatchIsRewrittenInPlace(t *testing.T) {
	for i, name := range remoteCeilingNames("you@github") {
		hub := newFakeAdminHub()
		hub.constraints = []*fakeConstraint{ceilingFor("ac-old", name, "user-1", append(webCeiling(), projectCreatePermission))}
		warned, warn := collectWarnings()
		id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
		if err != nil {
			t.Fatalf("name %d: %v", i, err)
		}
		c := hub.constraints[0]
		if id != "ac-old" || len(hub.constraints) != 1 || c.Name != name || c.revision != 4 ||
			!hubapi.SamePermissions(c.MaximumPermissions, webCeiling()) || c.Purpose != remoteCeilingPurpose {
			t.Fatalf("name %d: id %q, constraints %+v", i, id, hub.constraints)
		}
		if hub.count("POST /api/v1/admin/access-constraints")+hub.count("DELETE ") != 0 || hub.count("PUT /api/v1/admin/access-constraints/ac-old") != 1 {
			t.Fatalf("name %d: calls %v", i, hub.calls)
		}
		if len(*warned) != 1 || !strings.Contains((*warned)[0], "rewriting") {
			t.Fatalf("name %d: warnings %q", i, *warned)
		}
	}
}

// A ceiling on another hub user (the login's row was recreated) is not the
// user's: the new one is created under the free name before the old one is
// deleted, so the user is never uncapped. Run again, the names alternate.
func TestRemoteCeilingOnAnotherUserIsReplacedCreateFirst(t *testing.T) {
	names := remoteCeilingNames("you@github")
	hub := newFakeAdminHub()
	hub.constraints = []*fakeConstraint{ceilingFor("ac-old", names[0], "user-old", webCeiling())}
	warned, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	iCreate, iDelete := hub.callAt("POST /api/v1/admin/access-constraints"), hub.callAt("DELETE /api/v1/admin/access-constraints/ac-old")
	if iCreate < 0 || iDelete < 0 || iCreate > iDelete {
		t.Fatalf("create at %d, delete at %d; want the create first: %v", iCreate, iDelete, hub.calls)
	}
	if len(hub.constraints) != 1 || hub.constraints[0].id != id || hub.constraints[0].Name != names[1] ||
		hub.constraints[0].Subject.PrincipalID != "user-1" {
		t.Fatalf("constraints = %+v, id %q", hub.constraints, id)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "ac-old") {
		t.Fatalf("warnings = %q", *warned)
	}

	hub.constraints[0].Subject.PrincipalID = "user-old"
	old := hub.constraints[0].id
	hub.calls = nil
	id2, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	iCreate, iDelete = hub.callAt("POST /api/v1/admin/access-constraints"), hub.callAt("DELETE /api/v1/admin/access-constraints/"+old)
	if iCreate < 0 || iDelete < 0 || iCreate > iDelete {
		t.Fatalf("second replacement: create at %d, delete at %d: %v", iCreate, iDelete, hub.calls)
	}
	if len(hub.constraints) != 1 || hub.constraints[0].id != id2 || hub.constraints[0].Name != names[0] {
		t.Fatalf("second replacement: constraints = %+v", hub.constraints)
	}
}

// olderScionCeiling is the web ceiling as a lever on an older scion wrote it:
// that scion's hub-member still carried user.list, which scion has since
// reserved, so no hub user holds it and none may delete a constraint naming it.
func olderScionCeiling() []string {
	return append(slices.DeleteFunc(webCeiling(), func(p string) bool { return p == "user.list" }), "user.list", "skill.read")
}

func currentCeiling() []string {
	return slices.DeleteFunc(webCeiling(), func(p string) bool { return p == "user.list" })
}

// A migrated instance: the ceiling an older lever wrote (as scion's dev user)
// names user.list, which lever-admin does not hold. It is rewritten in place,
// which only needs authority over the permissions it adds; nothing is deleted.
func TestRemoteCeilingFromAnOlderScionIsRewritten(t *testing.T) {
	hub := newFakeAdminHub()
	hub.actorLacks = []string{"user.list"}
	hub.constraints = []*fakeConstraint{ceilingFor("ac-dev", remoteCeilingName("you@github"), "user-1", olderScionCeiling())}
	_, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", currentCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ac-dev" || len(hub.constraints) != 1 || !hubapi.SamePermissions(hub.constraints[0].MaximumPermissions, currentCeiling()) {
		t.Fatalf("id %q, constraints %+v, calls %v", id, hub.constraints, hub.calls)
	}
}

// A match under the second name beside the older scion's ceiling under the
// first, which lever-admin may not delete as it stands. The match is kept; the other is narrowed by what
// lever-admin lacks, then deleted. A later change of the ceiling rewrites the
// survivor in place and needs no free name.
func TestRemoteCeilingBesideAnUndeletableOlderOneIsCleanedUp(t *testing.T) {
	names := remoteCeilingNames("you@github")
	hub := newFakeAdminHub()
	hub.actorLacks = []string{"user.list"}
	hub.constraints = []*fakeConstraint{
		ceilingFor("ac-dev", names[0], "user-1", olderScionCeiling()),
		ceilingFor("ac-b", names[1], "user-1", currentCeiling()),
	}
	warned, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", currentCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ac-b" || len(hub.constraints) != 1 || hub.constraints[0].id != "ac-b" || hub.count("POST /api/v1/admin/access-constraints") != 0 {
		t.Fatalf("id %q, constraints %+v, calls %v", id, hub.constraints, hub.calls)
	}
	del := "DELETE /api/v1/admin/access-constraints/ac-dev"
	iFirst, iNarrow := slices.Index(hub.calls, del), hub.callAt("PUT /api/v1/admin/access-constraints/ac-dev")
	iLast := -1
	for i, c := range hub.calls {
		if c == del {
			iLast = i
		}
	}
	if iFirst < 0 || iNarrow < iFirst || iLast < iNarrow || hub.count(del) != 2 {
		t.Fatalf("want delete (refused), narrow, delete: %v", hub.calls)
	}
	if slices.ContainsFunc(*warned, func(w string) bool { return strings.Contains(w, "stays") }) {
		t.Fatalf("warnings %q", *warned)
	}

	changed := append(currentCeiling(), "inbox.read")
	hub.calls = nil
	id, err = ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", changed, warn)
	if err != nil || id != "ac-b" || !hubapi.SamePermissions(hub.constraints[0].MaximumPermissions, changed) || hub.count("POST /api/v1/admin/access-constraints") != 0 {
		t.Fatalf("later change: id %q, err %v, constraints %+v, calls %v", id, err, hub.constraints, hub.calls)
	}
}

// Both names held by the user's own outdated ceilings: one is rewritten, the
// other removed. While both stand the user holds no more than either.
func TestRemoteCeilingTwoOutdatedOwnCeilings(t *testing.T) {
	names := remoteCeilingNames("you@github")
	hub := newFakeAdminHub()
	hub.actorLacks = []string{"user.list"}
	hub.constraints = []*fakeConstraint{
		ceilingFor("ac-1", names[0], "user-1", olderScionCeiling()),
		ceilingFor("ac-2", names[1], "user-1", append(currentCeiling(), projectCreatePermission)),
	}
	_, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", currentCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ac-1" || len(hub.constraints) != 1 || !hubapi.SamePermissions(hub.constraints[0].MaximumPermissions, currentCeiling()) {
		t.Fatalf("id %q, constraints %+v, calls %v", id, hub.constraints, hub.calls)
	}
}

// A constraint lever-admin may neither delete nor narrow into one it may
// delete stays, with a warning: it only takes permissions away.
func TestRemoteCeilingUndeletableStaysWithAWarning(t *testing.T) {
	names := remoteCeilingNames("you@github")
	hub := newFakeAdminHub()
	hub.actorLacks = []string{"user.list"}
	hub.constraints = []*fakeConstraint{
		ceilingFor("ac-x", names[0], "user-old", []string{"user.list"}),
		ceilingFor("ac-b", names[1], "user-1", currentCeiling()),
	}
	warned, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", currentCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ac-b" || len(hub.constraints) != 2 || hub.count("PUT ") != 0 {
		t.Fatalf("id %q, constraints %+v, calls %v", id, hub.constraints, hub.calls)
	}
	if !slices.ContainsFunc(*warned, func(w string) bool { return strings.Contains(w, "ac-x") && strings.Contains(w, "stays") }) {
		t.Fatalf("warnings %q", *warned)
	}
}

// A rewrite that would add a permission lever-admin does not hold is refused;
// the narrowing part is still made, and the apply fails.
func TestRemoteCeilingRefusedWideningStillNarrows(t *testing.T) {
	hub := newFakeAdminHub()
	hub.actorLacks = []string{"agent.attach"}
	old := append(slices.DeleteFunc(currentCeiling(), func(p string) bool { return p == "agent.attach" }), projectCreatePermission)
	hub.constraints = []*fakeConstraint{ceilingFor("ac-1", remoteCeilingName("you@github"), "user-1", old)}
	_, warn := collectWarnings()
	_, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", currentCeiling(), warn)
	if err == nil || !strings.Contains(err.Error(), "agent.attach") {
		t.Fatalf("err = %v", err)
	}
	got := hub.constraints[0].MaximumPermissions
	if len(hub.constraints) != 1 || slices.Contains(got, projectCreatePermission) || slices.Contains(got, "agent.attach") {
		t.Fatalf("constraints %+v", hub.constraints)
	}
}

// A matching ceiling under one name and a stale one under the other: the
// match is kept, the stale one deleted, nothing created.
func TestRemoteCeilingStaleBesideAMatchIsDeleted(t *testing.T) {
	names := remoteCeilingNames("you@github")
	hub := newFakeAdminHub()
	hub.constraints = []*fakeConstraint{
		ceilingFor("ac-stale", names[0], "user-1", append(webCeiling(), projectCreatePermission)),
		ceilingFor("ac-keep", names[1], "user-1", webCeiling()),
	}
	_, warn := collectWarnings()
	id, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ac-keep" || hub.count("POST /api/v1/admin/access-constraints") != 0 ||
		len(hub.constraints) != 1 || hub.constraints[0].id != "ac-keep" {
		t.Fatalf("id %q, constraints %+v, calls %v", id, hub.constraints, hub.calls)
	}
}

// Both names held by constraints on other hub users leaves no free name: an
// error, and nothing is deleted, so the existing caps stay in force.
func TestRemoteCeilingBothNamesTakenFails(t *testing.T) {
	names := remoteCeilingNames("you@github")
	hub := newFakeAdminHub()
	hub.constraints = []*fakeConstraint{
		ceilingFor("ac-1", names[0], "user-older", webCeiling()),
		ceilingFor("ac-2", names[1], "user-old", webCeiling()),
	}
	_, warn := collectWarnings()
	_, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("err = %v, want both names reported taken", err)
	}
	if hub.count("POST ")+hub.count("DELETE ")+hub.count("PUT ") != 0 || len(hub.constraints) != 2 {
		t.Fatalf("wrote with no free name: %v", hub.calls)
	}
}

// A recovery-disabled constraint is scion's offline-recovery state: refused,
// untouched.
func TestRemoteCeilingRecoveryDisabledIsRefused(t *testing.T) {
	hub := newFakeAdminHub()
	c := ceilingFor("ac-rec", remoteCeilingName("you@github"), "user-1", webCeiling())
	c.status = "recovery_disabled"
	hub.constraints = []*fakeConstraint{c}
	_, warn := collectWarnings()
	_, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err == nil || !strings.Contains(err.Error(), "recovery-disabled") {
		t.Fatalf("err = %v", err)
	}
	if hub.count("POST ")+hub.count("DELETE ") != 0 {
		t.Fatalf("wrote: %v", hub.calls)
	}
}

// A rewrite the hub refuses (a blocked preview) fails before any delete: the
// stale ceiling stays.
func TestRemoteCeilingFailedRewriteDeletesNothing(t *testing.T) {
	hub := newFakeAdminHub()
	hub.constraints = []*fakeConstraint{ceilingFor("ac-old", remoteCeilingName("you@github"), "user-1", append(webCeiling(), projectCreatePermission))}
	hub.blockPreview = true
	_, warn := collectWarnings()
	_, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: hub}, "you@github", "user-1", webCeiling(), warn)
	if err == nil || !strings.Contains(err.Error(), "preview is incomplete") {
		t.Fatalf("err = %v", err)
	}
	if hub.count("DELETE ") != 0 || len(hub.constraints) != 1 || hub.constraints[0].id != "ac-old" {
		t.Fatalf("the stale ceiling was deleted without a replacement: %v", hub.calls)
	}
}

// A hub that refuses the constraint read fails the ceiling: lever never
// writes a ceiling it could not compare with what is there.
func TestRemoteCeilingHubRefusalFails(t *testing.T) {
	_, warn := collectWarnings()
	if _, err := ensureRemoteCeiling(context.Background(), &hubapi.Client{T: failingDoer{}}, "you@github", "user-1", webCeiling(), warn); err == nil {
		t.Fatal("a 403 on the constraint list must fail the ceiling")
	}
}

type failingDoer struct{}

func (failingDoer) Do(context.Context, string, string) (int, []byte, error) {
	return 403, []byte(`{"error":"forbidden"}`), nil
}

func (failingDoer) DoBody(ctx context.Context, m, p string, _ []byte) (int, []byte, error) {
	return failingDoer{}.Do(ctx, m, p)
}

// roleIDNamed is the id of the lever role called name on the fake hub.
func (h *fakeAdminHub) roleIDNamed(name string) string {
	for _, r := range h.roles {
		if r.Name == name {
			return r.ID
		}
	}
	return ""
}

// boundTo lists the role names userID is bound to.
func (h *fakeAdminHub) boundTo(userID string) []string {
	var out []string
	for _, b := range h.bindings {
		if b.PrincipalID != userID {
			continue
		}
		for _, r := range h.roles {
			if r.ID == b.RoleDefinitionID {
				out = append(out, r.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}
