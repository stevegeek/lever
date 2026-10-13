package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// hiHub is an in-memory scion hub in proxy auth mode with invite_only and
// lever-admin in admin_emails, as far as the hub-identity step and the doctor
// rows reach it. Every mutation is appended to events, so tests can check
// order.
type hiHub struct {
	t   *testing.T
	mu  sync.Mutex
	now func() time.Time
	seq int

	users       []*hiUser
	roles       []hubapi.RoleDefinition
	bindings    []hubapi.RoleBinding
	constraints []*hiConstraint
	previews    map[string]string
	tokens      []*hiToken
	projects    []hubapi.Project
	agents      []hiAgent
	scopeActs   []string

	// cache models the hub's email-to-user cache: an email keeps resolving to
	// a row for N more assertions.
	cache map[string]hiCache
	// staleAfterRename is the N a system/identity rename leaves on the old
	// address.
	staleAfterRename int
	// resolveSuspended lets /auth/me answer for a suspended row.
	resolveSuspended bool
	// keepRole lists users whose role a PATCH silently does not change.
	keepRole map[string]bool
	// down answers "METHOD /path" with the status, before any routing.
	down map[string]int

	events []string
}

type hiUser struct {
	ID, Email, Role, Status string
	Created                 time.Time
}

type hiConstraint struct {
	ID, Name  string
	Subject   hubapi.ConstraintSubject
	ScopeType string
	Perms     []string
	Rev       int
}

type hiToken struct {
	ID, Owner, Name, ProjectID, Secret string
	Scopes                             []string
	Expires                            time.Time
	Revoked                            bool
}

type hiAgent struct {
	Slug    string
	Actions []string
}

type hiCache struct {
	ID string
	N  int
}

const (
	hiSuperAdminRole = "role-super-admin"
	hiHubMemberRole  = "role-hub-member"
	hiJailMount      = "/home/lever.linux/lever"
	hiMarkerID       = "6f1c2d3e-4a5b-4c6d-8e7f-001122334455"
	hiKeyFP          = "sha256:test-key"
)

var hiHubMemberPerms = []string{"project.create", "project.list", "user.read"}

func newHIHub(t *testing.T, now func() time.Time) *hiHub {
	return &hiHub{
		t: t, now: now,
		roles: []hubapi.RoleDefinition{
			{ID: hiSuperAdminRole, Name: hubapi.SystemRoleSuperAdmin, ScopeType: hubapi.RoleScopeSystem, System: true, Permissions: []string{"*"}},
			{ID: hiHubMemberRole, Name: hubMemberRoleName, ScopeType: hubapi.RoleScopeSystem, System: true, Permissions: hiHubMemberPerms},
			{ID: "role-project-owner", Name: "project-owner", ScopeType: hubapi.RoleScopeProject, System: true, Permissions: []string{"project.update"}},
		},
		previews:  map[string]string{},
		cache:     map[string]hiCache{},
		keepRole:  map[string]bool{},
		down:      map[string]int{},
		scopeActs: []string{"create", "list"},
	}
}

func (h *hiHub) id(prefix string) string {
	h.seq++
	return fmt.Sprintf("%s-%d", prefix, h.seq)
}

func (h *hiHub) event(format string, args ...any) {
	h.events = append(h.events, fmt.Sprintf(format, args...))
}

// addUser adds a row; id "" makes one.
func (h *hiHub) addUser(id, email, role, status string, created time.Time) *hiUser {
	if id == "" {
		id = h.id("user")
	}
	u := &hiUser{ID: id, Email: email, Role: role, Status: status, Created: created}
	h.users = append(h.users, u)
	return u
}

func (h *hiHub) user(id string) *hiUser {
	for _, u := range h.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (h *hiHub) byEmail(email string) *hiUser {
	for _, u := range h.users {
		if strings.EqualFold(u.Email, email) {
			return u
		}
	}
	return nil
}

func (h *hiHub) removeUser(id string) {
	h.users = slices.DeleteFunc(h.users, func(u *hiUser) bool { return u.ID == id })
}

func (h *hiHub) roleName(id string) string {
	for _, r := range h.roles {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

func (h *hiHub) roleNamed(name string) (hubapi.RoleDefinition, bool) {
	for _, r := range h.roles {
		if r.Name == name {
			return r, true
		}
	}
	return hubapi.RoleDefinition{}, false
}

func (h *hiHub) addBinding(roleID, userID, scopeType, scopeID, createdBy string) {
	h.bindings = append(h.bindings, hubapi.RoleBinding{ID: h.id("rb"), RoleDefinitionID: roleID,
		PrincipalType: hubapi.PrincipalUser, PrincipalID: userID, ScopeType: scopeType, ScopeID: scopeID, CreatedBy: createdBy})
}

// boundTo lists the role names userID holds directly.
func (h *hiHub) boundTo(userID string) []string {
	var out []string
	for _, b := range h.bindings {
		if b.PrincipalID == userID {
			out = append(out, h.roleName(b.RoleDefinitionID))
		}
	}
	return out
}

func (h *hiHub) ceilingOn(userID string) *hiConstraint {
	for _, c := range h.constraints {
		if c.Subject.PrincipalID == userID {
			return c
		}
	}
	return nil
}

// at is the index of the first event equal to ev, or -1.
func (h *hiHub) at(ev string) int { return slices.Index(h.events, ev) }

func (h *hiHub) eventsFrom(n int) []string { return slices.Clone(h.events[n:]) }

// hiCaller is who a request comes from: an assertion's email or a PAT.
type hiCaller struct {
	email  string
	secret string
}

// hiDoer is the in-process transport for one identity. An assertion doer
// enforces AssertCurl's route allowlist, so a step that would send an
// assertion to a route AssertCurl refuses fails here too.
type hiDoer struct {
	h     *hiHub
	email string
	pat   func() string
}

func (d hiDoer) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return d.DoBody(ctx, method, path, nil)
}

func (d hiDoer) DoBody(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	c := hiCaller{email: d.email}
	if d.email != "" {
		if err := hubapi.AssertionPathAllowed(method, path); err != nil {
			d.h.t.Errorf("an assertion was sent to a route AssertCurl refuses: %v", err)
			return 0, nil, err
		}
	} else {
		if d.pat != nil {
			c.secret = d.pat()
		}
		if c.secret == "" {
			return 0, nil, fmt.Errorf("no controller PAT available for %s %s", method, path)
		}
	}
	st, b := d.h.handle(c, method, path, body)
	return st, b, nil
}

func hiJSON(status int, v any) (int, []byte) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return status, b
}

func hiErr(status int, msg string) (int, []byte) {
	return hiJSON(status, map[string]string{"error": msg})
}

func hiUserWire(u *hiUser) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "role": u.Role, "status": u.Status, "created": u.Created}
}

// resolve answers who an assertion is, the way scion's proxy auth does under
// invite_only with lever-admin in admin_emails.
func (h *hiHub) resolve(email string) (*hiUser, int) {
	e := strings.ToLower(email)
	if c, ok := h.cache[e]; ok && c.N > 0 {
		c.N--
		h.cache[e] = c
		if u := h.user(c.ID); u != nil {
			return u, 0
		}
	}
	u := h.byEmail(e)
	if u == nil {
		if e != hubauth.AdminEmail {
			return nil, http.StatusForbidden
		}
		u = h.addUser("", e, "admin", "active", h.now())
		h.addBinding(hiSuperAdminRole, u.ID, hubapi.RoleScopeSystem, "", "system-reconcile")
		h.event("admin row %s", u.ID)
		return u, 0
	}
	switch u.Status {
	case "suspended":
		if !h.resolveSuspended {
			return nil, http.StatusForbidden
		}
	case "invited":
		u.Status = "active"
		h.event("signin %s", u.ID)
	}
	return u, 0
}

func (h *hiHub) handle(c hiCaller, method, raw string, body []byte) (int, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil {
		return hiErr(400, err.Error())
	}
	p, q := u.Path, u.Query()
	if st, ok := h.down[method+" "+p]; ok {
		return hiErr(st, "down")
	}
	pat := c.email == ""
	var me *hiUser
	if pat {
		for _, t := range h.tokens {
			if t.Secret == c.secret && !t.Revoked {
				me = h.user(t.Owner)
			}
		}
		if me == nil {
			return hiErr(401, "invalid token")
		}
	} else {
		var st int
		if me, st = h.resolve(c.email); me == nil {
			return hiErr(st, "email not authorized")
		}
	}
	adminOnly := strings.HasPrefix(p, "/api/v1/admin/") || p == "/api/v1/users" || strings.HasPrefix(p, "/api/v1/users/")
	if adminOnly && me.Role != "admin" {
		return hiErr(403, "admin only")
	}
	var in map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return hiErr(400, err.Error())
		}
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	tail := func(prefix string) (string, bool) {
		rest, ok := strings.CutPrefix(p, prefix)
		return rest, ok && rest != "" && !strings.Contains(rest, "/")
	}

	switch {
	case method == http.MethodGet && p == "/api/v1/auth/me":
		return hiJSON(200, hiUserWire(me))

	case method == http.MethodGet && p == "/api/v1/users":
		var out []map[string]any
		for _, x := range h.users {
			if r := q.Get("role"); r != "" && x.Role != r {
				continue
			}
			if s := q.Get("search"); s != "" && !strings.Contains(strings.ToLower(x.Email), strings.ToLower(s)) {
				continue
			}
			out = append(out, hiUserWire(x))
		}
		return hiJSON(200, map[string]any{"users": out, "nextCursor": ""})

	case method == http.MethodPost && p == "/api/v1/admin/users/invite":
		email := str("email")
		if h.byEmail(email) != nil {
			return hiErr(409, "user exists")
		}
		x := h.addUser("", email, "member", "invited", h.now())
		h.event("invite %s %s", email, x.ID)
		return hiJSON(201, map[string]any{"id": x.ID, "email": x.Email, "status": x.Status, "created": x.Created})
	}
	if id, ok := tail("/api/v1/users/"); ok {
		x := h.user(id)
		if x == nil {
			return hiErr(404, "no user")
		}
		switch method {
		case http.MethodGet:
			return hiJSON(200, hiUserWire(x))
		case http.MethodPatch:
			role, status := str("role"), str("status")
			if role != "" && x.Status == "invited" {
				return hiErr(409, "role is assigned at first sign-in")
			}
			if role != "" && !h.keepRole[id] {
				x.Role = role
				if role != "admin" {
					h.bindings = slices.DeleteFunc(h.bindings, func(b hubapi.RoleBinding) bool {
						return b.PrincipalID == id && b.RoleDefinitionID == hiSuperAdminRole
					})
				}
			}
			if status != "" {
				x.Status = status
			}
			h.event("patch %s role=%s status=%s", id, role, status)
			return hiJSON(200, hiUserWire(x))
		}
	}

	switch {
	case method == http.MethodGet && p == "/api/v1/admin/roles":
		return hiJSON(200, map[string]any{"items": h.roles})
	case method == http.MethodPost && p == "/api/v1/admin/roles":
		var rd hubapi.RoleDefinition
		_ = json.Unmarshal(body, &rd)
		rd.ID = h.id("role")
		h.roles = append(h.roles, rd)
		h.event("role+ %s %s", rd.Name, rd.ID)
		return hiJSON(201, rd)
	}
	if id, ok := tail("/api/v1/admin/roles/"); ok && method == http.MethodPut {
		for i, r := range h.roles {
			if r.ID == id {
				var rd hubapi.RoleDefinition
				_ = json.Unmarshal(body, &rd)
				h.roles[i].Permissions = rd.Permissions
				h.event("role~ %s", r.Name)
				return hiJSON(200, h.roles[i])
			}
		}
		return hiErr(404, "no role")
	}

	switch {
	case method == http.MethodGet && p == "/api/v1/admin/role-bindings":
		var items []hubapi.RoleBinding
		for _, b := range h.bindings {
			if pid := q.Get("principalId"); pid != "" && b.PrincipalID != pid {
				continue
			}
			items = append(items, b)
		}
		total := len(items)
		if off, err := strconv.Atoi(q.Get("offset")); err == nil {
			items = items[min(off, len(items)):]
		}
		if items == nil {
			items = []hubapi.RoleBinding{}
		}
		return hiJSON(200, map[string]any{"items": items, "totalCount": total})
	case method == http.MethodPost && p == "/api/v1/admin/role-bindings":
		var rb hubapi.RoleBinding
		_ = json.Unmarshal(body, &rb)
		for _, b := range h.bindings {
			if b.RoleDefinitionID == rb.RoleDefinitionID && b.PrincipalID == rb.PrincipalID && b.ScopeType == rb.ScopeType && b.ScopeID == rb.ScopeID {
				return hiErr(409, "binding exists")
			}
		}
		rb.ID, rb.CreatedBy = h.id("rb"), me.ID
		h.bindings = append(h.bindings, rb)
		h.event("bind %s %s", h.roleName(rb.RoleDefinitionID), rb.PrincipalID)
		return hiJSON(201, rb)
	}
	if id, ok := tail("/api/v1/admin/role-bindings/"); ok && method == http.MethodDelete {
		for i, b := range h.bindings {
			if b.ID == id {
				h.bindings = slices.Delete(h.bindings, i, i+1)
				h.event("unbind %s %s", h.roleName(b.RoleDefinitionID), b.PrincipalID)
				return 204, nil
			}
		}
		return hiErr(404, "no binding")
	}

	if st, b, ok := h.constraintRoute(method, p, q, body); ok {
		return st, b
	}

	switch {
	case method == http.MethodPost && p == "/api/v1/auth/tokens":
		if pat {
			return hiErr(403, "access tokens cannot manage other access tokens")
		}
		var req struct {
			Name      string   `json:"name"`
			ProjectID string   `json:"projectId"`
			Scopes    []string `json:"scopes"`
			ExpiresAt string   `json:"expiresAt"`
		}
		_ = json.Unmarshal(body, &req)
		exp, _ := time.Parse(time.RFC3339, req.ExpiresAt)
		t := &hiToken{ID: h.id("tok"), Owner: me.ID, Name: req.Name, ProjectID: req.ProjectID, Scopes: req.Scopes, Expires: exp}
		t.Secret = "scion_pat_" + t.ID
		h.tokens = append(h.tokens, t)
		h.event("mint %s by %s", req.Name, me.ID)
		return hiJSON(201, map[string]any{"token": t.Secret, "accessToken": hubapi.Token{ID: t.ID, Name: t.Name,
			ProjectID: t.ProjectID, Scopes: t.Scopes, ExpiresAt: t.Expires}})
	case method == http.MethodGet && p == "/api/v1/auth/tokens":
		if pat {
			return hiErr(403, "access tokens cannot manage other access tokens")
		}
		items := []hubapi.Token{}
		for _, t := range h.tokens {
			if t.Owner == me.ID {
				items = append(items, hubapi.Token{ID: t.ID, Name: t.Name, ProjectID: t.ProjectID, Scopes: t.Scopes, Revoked: t.Revoked, ExpiresAt: t.Expires})
			}
		}
		return hiJSON(200, map[string]any{"items": items})
	case method == http.MethodPost && strings.HasPrefix(p, "/api/v1/auth/tokens/") && strings.HasSuffix(p, "/revoke"):
		if pat {
			return hiErr(403, "access tokens cannot manage other access tokens")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/auth/tokens/"), "/revoke")
		for _, t := range h.tokens {
			if t.ID == id && t.Owner == me.ID {
				t.Revoked = true
				h.event("revoke %s", id)
				return 204, nil
			}
		}
		return hiErr(404, "no token")

	case method == http.MethodPost && p == "/api/v1/projects/register":
		if pat {
			return hiErr(403, "a token cannot register a project")
		}
		id, name := str("id"), str("name")
		for _, pr := range h.projects {
			if pr.ID == id || pr.Slug == name {
				return hiJSON(200, map[string]any{"project": pr, "created": false})
			}
		}
		pr := hubapi.Project{ID: id, Name: name, Slug: name, OwnerID: me.ID, CreatedBy: me.ID}
		h.projects = append(h.projects, pr)
		h.event("register %s by %s", id, me.ID)
		return hiJSON(201, map[string]any{"project": pr, "created": true})
	case method == http.MethodGet && p == "/api/v1/projects":
		return hiJSON(200, map[string]any{"projects": h.projects})
	case method == http.MethodGet && strings.HasPrefix(p, "/api/v1/projects/"):
		rest := strings.TrimPrefix(p, "/api/v1/projects/")
		id, sub, _ := strings.Cut(rest, "/")
		for _, pr := range h.projects {
			if pr.ID != id {
				continue
			}
			switch sub {
			case "":
				return hiJSON(200, pr)
			case "shared-dirs":
				return hiJSON(200, map[string]any{"sharedDirs": []hubapi.SharedDir{}})
			}
		}
		return hiErr(404, "no project")

	case method == http.MethodGet && p == "/api/v1/agents":
		h.event("agents by %s", me.ID)
		var out []map[string]any
		for _, a := range h.agents {
			out = append(out, map[string]any{"slug": a.Slug, "_capabilities": map[string]any{"actions": a.Actions}})
		}
		return hiJSON(200, map[string]any{"agents": out, "_capabilities": map[string]any{"actions": h.scopeActs}})

	case method == http.MethodPut && p == "/api/v1/system/identity":
		email := str("email")
		dev := h.user(devUserID)
		if dev == nil {
			return hiJSON(200, map[string]any{})
		}
		if other := h.byEmail(email); other != nil && other.ID != devUserID {
			h.event("identity %s ignored by %s", email, me.ID)
			return hiJSON(200, map[string]any{})
		}
		old := strings.ToLower(dev.Email)
		dev.Email = email
		if h.staleAfterRename > 0 && old != strings.ToLower(email) {
			h.cache[old] = hiCache{ID: devUserID, N: h.staleAfterRename}
		}
		h.event("identity %s by %s", email, me.ID)
		return hiJSON(200, map[string]any{})
	}
	h.t.Errorf("fake hub: no route for %s %s", method, raw)
	return hiErr(404, "no route")
}

func (h *hiHub) constraintRoute(method, p string, q url.Values, body []byte) (int, []byte, bool) {
	const base = "/api/v1/admin/access-constraints"
	wire := func(c *hiConstraint, detail bool) map[string]any {
		m := map[string]any{"id": c.ID, "name": c.Name, "subject": c.Subject,
			"scope": map[string]string{"type": c.ScopeType}, "status": "active", "revision": strconv.Itoa(c.Rev)}
		if detail {
			perms := []map[string]string{}
			for _, x := range c.Perms {
				perms = append(perms, map[string]string{"id": x})
			}
			m["maximumPermissions"] = perms
		}
		return m
	}
	consume := func(tok, want string) bool {
		got, ok := h.previews[tok]
		delete(h.previews, tok)
		return ok && got == want
	}
	switch {
	case method == http.MethodPost && p == "/api/v1/admin/access-constraint-previews":
		var req struct {
			Operation    string                  `json:"operation"`
			Draft        *hubapi.ConstraintDraft `json:"draft"`
			ConstraintID string                  `json:"constraintId"`
		}
		_ = json.Unmarshal(body, &req)
		tok := h.id("preview")
		h.previews[tok] = req.Operation + ":" + req.ConstraintID
		st, b := hiJSON(200, map[string]any{"previewToken": tok})
		return st, b, true
	case method == http.MethodGet && p == base:
		items := []map[string]any{}
		for _, c := range h.constraints {
			if strings.Contains(c.Name, q.Get("nameContains")) {
				items = append(items, wire(c, false))
			}
		}
		st, b := hiJSON(200, map[string]any{"items": items})
		return st, b, true
	case method == http.MethodPost && p == base:
		var req struct {
			hubapi.ConstraintDraft
			PreviewToken string `json:"previewToken"`
		}
		_ = json.Unmarshal(body, &req)
		if !consume(req.PreviewToken, "create:") {
			st, b := hiErr(409, "bad preview token")
			return st, b, true
		}
		for _, c := range h.constraints {
			if c.Name == req.Name {
				st, b := hiErr(409, "name taken")
				return st, b, true
			}
		}
		c := &hiConstraint{ID: h.id("ac"), Name: req.Name, Subject: req.Subject, ScopeType: req.Scope.Type, Perms: req.MaximumPermissions, Rev: 1}
		h.constraints = append(h.constraints, c)
		h.event("ceiling+ %s %s", c.Subject.PrincipalID, strings.Join(c.Perms, ","))
		st, b := hiJSON(201, wire(c, true))
		return st, b, true
	}
	id, ok := strings.CutPrefix(p, base+"/")
	if !ok {
		return 0, nil, false
	}
	for i, c := range h.constraints {
		if c.ID != id {
			continue
		}
		switch method {
		case http.MethodGet:
			st, b := hiJSON(200, wire(c, true))
			return st, b, true
		case http.MethodDelete:
			var req struct {
				PreviewToken string `json:"previewToken"`
			}
			_ = json.Unmarshal(body, &req)
			if !consume(req.PreviewToken, "delete:"+id) {
				st, b := hiErr(409, "bad preview token")
				return st, b, true
			}
			h.constraints = slices.Delete(h.constraints, i, i+1)
			h.event("ceiling- %s %s", c.Subject.PrincipalID, strings.Join(c.Perms, ","))
			return 204, nil, true
		}
	}
	st, b := hiErr(404, "no constraint")
	return st, b, true
}

// hiScion is the jail's scion as the step drives it.
type hiScion struct {
	h         *hiHub
	st        state.State
	lifecycle bool
	listErr   error
}

func (s *hiScion) InitProject(_ context.Context, dir string) error {
	s.h.event("scion init %s", dir)
	return nil
}

func (s *hiScion) HubLink(_ context.Context, dir string) error {
	if tok, _ := s.st.LoadControllerPAT(); tok == "" {
		return errors.New("hub link: no controller PAT on disk")
	}
	s.h.event("scion link %s", dir)
	return nil
}

func (s *hiScion) KnowsUATScope(_ context.Context, scope string) (bool, error) {
	return scope == lifecycleScope && s.lifecycle, nil
}

func (s *hiScion) List(context.Context, string) ([]scion.Agent, error) {
	s.h.event("scion list")
	return nil, s.listErr
}

// hiJailRunner answers the jail commands the step and the doctor rows run.
// It fails the test on anything else, and on any podman command: the step
// must never list containers.
type hiJailRunner struct {
	t      *testing.T
	marker string
	// curl, when set, answers AssertCurl's in-jail curl and the doctor's
	// plain curl and jail reads.
	curl func(env map[string]string, name string, args []string) (proc.Result, bool)
}

func (r *hiJailRunner) Run(_ context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv := name + " " + strings.Join(args, " ")
	if strings.Contains(argv, "podman") {
		r.t.Errorf("the hub identity code ran %q; it must never touch containers", name)
		return proc.Result{Code: 1}, errors.New("podman refused by the test")
	}
	if name == "sh" && len(args) == 4 && args[0] == "-c" && args[2] == "_" && args[3] == hiJailMount+"/.scion" {
		if r.marker == "" {
			return proc.Result{Code: 1}, errors.New("exit status 1")
		}
		return proc.Result{Stdout: r.marker}, nil
	}
	if r.curl != nil {
		if res, ok := r.curl(env, name, args); ok {
			if res.Code != 0 {
				return res, fmt.Errorf("exit status %d", res.Code)
			}
			return res, nil
		}
	}
	r.t.Errorf("unexpected jail command %q", name+" "+hiFirstArg(args))
	return proc.Result{Code: 1}, errors.New("unexpected command")
}

func hiFirstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (r *hiJailRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.Run(ctx, env, name, args...)
}

func (r *hiJailRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.Run(ctx, env, name, args...)
}

// hiEnv is one instance's host state and jail around a fake hub.
type hiEnv struct {
	t      *testing.T
	app    *config.App
	now    time.Time
	st     state.State
	hub    *hiHub
	sc     *hiScion
	jr     *hiJailRunner
	reset  bool
	logs   []string
	warns  []string
	sleeps []time.Duration
	keyFP  string
	// cacheWait and cachePoll go to the step; zero keeps its defaults.
	cacheWait, cachePoll time.Duration
}

func newHIEnv(t *testing.T, app *config.App) *hiEnv {
	e := &hiEnv{t: t, app: app, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
		st: state.State{Dir: t.TempDir()}, keyFP: hiKeyFP, cacheWait: 40 * time.Millisecond, cachePoll: 10 * time.Millisecond}
	e.jr = &hiJailRunner{t: t, marker: "project-id: " + hiMarkerID + "\nproject-slug: lever\n"}
	e.newHub()
	return e
}

// newHub swaps in an empty hub database.
func (e *hiEnv) newHub() *hiHub {
	seq := 0
	if e.hub != nil {
		seq = e.hub.seq + 1000
	}
	e.hub = newHIHub(e.t, func() time.Time { return e.now })
	e.hub.seq = seq
	e.sc = &hiScion{h: e.hub, st: e.st}
	return e.hub
}

func (e *hiEnv) step() *hubIdentity {
	return &hubIdentity{
		app: e.app, st: e.st, jr: e.jr, sc: e.sc,
		admin: &hubapi.Client{T: hiDoer{h: e.hub, email: hubauth.AdminEmail}},
		ctl:   &hubapi.Client{T: hiDoer{h: e.hub, email: hubauth.ControllerEmail}},
		pat: &hubapi.Client{T: hiDoer{h: e.hub, pat: func() string {
			tok, _ := e.st.LoadControllerPAT()
			return tok
		}}},
		jailMount: hiJailMount + "/",
		keyFP:     e.keyFP,
		guestNow:  func() time.Time { return e.now },
		reset:     e.reset,
		log:       func(f string, a ...any) { e.logs = append(e.logs, fmt.Sprintf(f, a...)) },
		warn:      func(f string, a ...any) { e.warns = append(e.warns, fmt.Sprintf(f, a...)) },
		sleep: func(ctx context.Context, d time.Duration) error {
			e.sleeps = append(e.sleeps, d)
			e.now = e.now.Add(d)
			return ctx.Err()
		},
		cacheWait: e.cacheWait, cachePoll: e.cachePoll,
	}
}

func (e *hiEnv) run() error { return e.step().run(context.Background()) }

func (e *hiEnv) mustRun() {
	e.t.Helper()
	if err := e.run(); err != nil {
		e.t.Fatalf("run: %v\nevents: %q", err, e.hub.events)
	}
}

func (e *hiEnv) record() state.HubIdentity {
	e.t.Helper()
	rec, found, err := e.st.LoadHubIdentity()
	if err != nil || !found {
		e.t.Fatalf("hub-identity.json: found=%v %v", found, err)
	}
	return rec
}

func (e *hiEnv) remoteRole() state.RemoteRoleRecord {
	e.t.Helper()
	rr, found, err := e.st.LoadRemoteRoleRecord()
	if err != nil || !found {
		e.t.Fatalf("remote-role.json: found=%v %v", found, err)
	}
	return rr
}

// diskPATIs reports whether the controller PAT on disk is the hub token id's
// secret, without printing either.
func (e *hiEnv) diskPATIs(load func() (string, error), tokID string) bool {
	tok, err := load()
	if err != nil || tok == "" {
		return false
	}
	for _, t := range e.hub.tokens {
		if t.ID == tokID {
			return t.Secret == tok
		}
	}
	return false
}

// seedMigrated is an instance an older lever brought up with dev-auth
// windows: scion's dev user is an admin at devEmail, owns the project, holds
// lever-controller and issued the controller PAT lever holds.
func (e *hiEnv) seedMigrated(devEmail string, lifecycle bool) {
	h := e.hub
	h.addUser(devUserID, devEmail, "admin", "active", e.now.Add(-60*24*time.Hour))
	h.addBinding(hiSuperAdminRole, devUserID, hubapi.RoleScopeSystem, "", "system-reconcile")
	h.projects = append(h.projects, hubapi.Project{ID: "proj-1", Name: "lever", Slug: "lever", OwnerID: devUserID, CreatedBy: devUserID})
	h.roles = append(h.roles, hubapi.RoleDefinition{ID: "role-ctl-old", Name: controllerRoleName, Description: controllerRoleDescription,
		ScopeType: hubapi.RoleScopeProject, Permissions: controllerRolePermissions(lifecycle)})
	h.addBinding("role-ctl-old", devUserID, hubapi.RoleScopeProject, "proj-1", devUserID)
	h.tokens = append(h.tokens, &hiToken{ID: "tok-old", Owner: devUserID, Name: "lever-controller", ProjectID: "proj-1",
		Secret: "scion_pat_old", Scopes: controllerPATScopes(lifecycle), Expires: e.now.Add(300 * 24 * time.Hour)})
	acts := controllerActions(lifecycle)
	h.agents = []hiAgent{{Slug: "manager", Actions: acts}, {Slug: "worker", Actions: acts}}
	e.sc.lifecycle = lifecycle
	if err := e.st.SaveControllerPAT("scion_pat_old"); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SaveControllerPATRecord(state.PATRecord{ID: "tok-old", Requested: controllerPATScopes(lifecycle),
		MintedAt: e.now.Add(-10 * 24 * time.Hour), ExpiresAt: e.now.Add(300 * 24 * time.Hour)}); err != nil {
		e.t.Fatal(err)
	}
}

func hiRemoteApp(enabled bool, users ...config.RemoteUser) *config.App {
	return &config.App{Remote: config.Remote{Enabled: enabled, AllowedUsers: users}}
}

func hiOperator(login string) config.RemoteUser { return config.RemoteUser{Login: login} }

func hiContact(login string) config.RemoteUser {
	return config.RemoteUser{Login: login, Tier: config.TierContact, Agents: []string{"manager"}}
}

// hiWantOrder fails unless the events occur in the order given, each after
// the one before it.
func hiWantOrder(t *testing.T, h *hiHub, evs ...string) {
	t.Helper()
	from := 0
	for _, ev := range evs {
		i := slices.Index(h.events[from:], ev)
		if i < 0 {
			t.Fatalf("event %q does not happen after event %d; events: %q", ev, from-1, h.events)
		}
		from += i + 1
	}
}

func hiEventsWith(evs []string, prefixes ...string) []string {
	var out []string
	for _, ev := range evs {
		for _, p := range prefixes {
			if strings.HasPrefix(ev, p) {
				out = append(out, ev)
			}
		}
	}
	return out
}

func hiAnyContains(lines []string, subs ...string) bool {
	for _, l := range lines {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(l, s)
		}
		if ok {
			return true
		}
	}
	return false
}

func hiTokenNamed(h *hiHub, name, owner string) *hiToken {
	for _, t := range h.tokens {
		if t.Name == name && t.Owner == owner && !t.Revoked {
			return t
		}
	}
	return nil
}

func TestHubIdentityFreshBootstrap(t *testing.T) {
	op, contact := "op@example.com", "c1@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	e.mustRun()
	h := e.hub

	admin, ctl := h.byEmail(hubauth.AdminEmail), h.byEmail(hubauth.ControllerEmail)
	if admin == nil || admin.Role != "admin" || admin.ID == devUserID {
		t.Fatalf("admin row = %+v", admin)
	}
	if ctl == nil || ctl.Role != "member" || ctl.Status != "active" || ctl.ID == admin.ID {
		t.Fatalf("controller row = %+v", ctl)
	}
	role, ok := h.roleNamed(controllerRoleName)
	if !ok {
		t.Fatal("no lever-controller role")
	}
	// C, D, E, C': link only once the controller PAT exists.
	hiWantOrder(t, h,
		"invite "+hubauth.ControllerEmail+" "+ctl.ID,
		"scion init "+hiJailMount,
		"register "+hiMarkerID+" by "+ctl.ID,
		"bind "+controllerRoleName+" "+ctl.ID,
		"mint lever-controller by "+ctl.ID,
		"mint lever-remote by "+ctl.ID,
		"scion link "+hiJailMount)
	if len(h.projects) != 1 || h.projects[0].OwnerID != ctl.ID || h.projects[0].Slug != "lever" {
		t.Fatalf("projects = %+v", h.projects)
	}
	ctlTok, remTok := hiTokenNamed(h, "lever-controller", ctl.ID), hiTokenNamed(h, "lever-remote", ctl.ID)
	if ctlTok == nil || remTok == nil || ctlTok.ProjectID != hiMarkerID ||
		!hubapi.SamePermissions(ctlTok.Scopes, controllerPATScopes(false)) || !hubapi.SamePermissions(remTok.Scopes, remotePATScopes()) {
		t.Fatalf("tokens = %+v", h.tokens)
	}
	if !e.diskPATIs(e.st.LoadControllerPAT, ctlTok.ID) || !e.diskPATIs(e.st.LoadRemotePAT, remTok.ID) {
		t.Fatal("the PATs on disk are not the ones minted")
	}
	if r, found, _ := e.st.LoadControllerPATRecord(); !found || r.ID != ctlTok.ID {
		t.Fatalf("controller PAT record = %+v found=%v", r, found)
	}

	rec := e.record()
	if rec.Mode != state.HubModeFresh || rec.AdminID != admin.ID || rec.ControllerID != ctl.ID ||
		rec.ProjectID != hiMarkerID || rec.HubMarker != role.ID || rec.KeyFingerprint != hiKeyFP {
		t.Fatalf("record = %+v", rec)
	}

	// Every login is invited, capped, then bound, and none has signed in.
	webRole, _ := h.roleNamed(remoteWebRoleName)
	contactRole, _ := h.roleNamed(contactRoleName)
	for email, rn := range map[string]string{op: remoteWebRoleName, contact: contactRoleName} {
		u := h.byEmail(email)
		if u == nil || u.Status != "invited" || rec.Users[email] != u.ID {
			t.Fatalf("%s: row %+v, record %q", email, u, rec.Users[email])
		}
		c := h.ceilingOn(u.ID)
		if c == nil || c.ScopeType != hubapi.ConstraintScopeSystem {
			t.Fatalf("%s: ceiling %+v", email, c)
		}
		hiWantOrder(t, h, "invite "+email+" "+u.ID, "ceiling+ "+u.ID+" "+strings.Join(c.Perms, ","), "bind "+rn+" "+u.ID)
		if got := h.boundTo(u.ID); !slices.Equal(got, []string{rn}) {
			t.Fatalf("%s bound to %q, want only %s", email, got, rn)
		}
	}
	if c := h.ceilingOn(h.byEmail(contact).ID); !slices.Equal(c.Perms, contactRolePermissions()) {
		t.Fatalf("contact ceiling = %v", c.Perms)
	}
	if c := h.ceilingOn(h.byEmail(op).ID); slices.Contains(c.Perms, projectCreatePermission) || !slices.Contains(c.Perms, "agent.attach") {
		t.Fatalf("operator ceiling = %v", c.Perms)
	}
	rr := e.remoteRole()
	if rr.RoleID != webRole.ID || rr.ContactRoleID != contactRole.ID || rr.ProjectID != hiMarkerID ||
		len(rr.Bound) != 2 || !slices.Equal(rr.Contacts, []string{contact}) {
		t.Fatalf("remote-role.json = %+v", rr)
	}

	// The second run finds everything in place.
	n := len(h.events)
	e.mustRun()
	if changed := hiEventsWith(h.eventsFrom(n), "invite", "register", "mint", "revoke", "scion", "bind", "unbind", "ceiling", "patch", "role", "identity"); len(changed) > 0 {
		t.Fatalf("second run changed the hub: %q", changed)
	}
	if again := e.record(); again.ControllerID != rec.ControllerID || again.AdminID != rec.AdminID || again.HubMarker != rec.HubMarker {
		t.Fatalf("second run record = %+v, first %+v", again, rec)
	}
}

func TestHubIdentityFreshNeedsTheProjectMarker(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.jr.marker = "project-slug: lever\n"
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "project marker") {
		t.Fatalf("run = %v, want a project marker error", err)
	}
	if len(e.hub.projects) != 0 || hiEventsWith(e.hub.events, "mint") != nil {
		t.Fatalf("nothing should be registered or minted: %q", e.hub.events)
	}
}

func TestHubIdentityMigrates(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.seedMigrated("scion@localhost", true)
	h := e.hub
	// An admin promoted through the API: member by role, admin by binding.
	eve := h.addUser("", "eve@example.com", "member", "active", e.now.Add(-time.Hour))
	h.addBinding(hiSuperAdminRole, eve.ID, hubapi.RoleScopeSystem, "", "admin-api")

	e.mustRun()

	admin := h.byEmail(hubauth.AdminEmail)
	if admin == nil || admin.ID == devUserID || admin.Role != "admin" {
		t.Fatalf("admin row = %+v", admin)
	}
	dev := h.user(devUserID)
	if dev.Role != "member" || dev.Email != hubauth.ControllerEmail || slices.Contains(h.boundTo(devUserID), hubapi.SystemRoleSuperAdmin) {
		t.Fatalf("dev row = %+v, bound to %q", dev, h.boundTo(devUserID))
	}
	if slices.Contains(h.boundTo(eve.ID), hubapi.SystemRoleSuperAdmin) {
		t.Fatalf("the API-promoted admin keeps %q", h.boundTo(eve.ID))
	}
	// A' then B (as lever-admin), then the probe as the demoted controller.
	hiWantOrder(t, h,
		"admin row "+admin.ID,
		"patch "+devUserID+" role=member status=",
		"identity "+hubauth.ControllerEmail+" by "+admin.ID,
		"agents by "+devUserID,
		"scion list")
	if h.at("patch "+eve.ID+" role=member status=") < 0 {
		t.Fatalf("eve was not demoted: %q", h.events)
	}
	if got := hiEventsWith(h.events, "mint", "register", "scion init", "scion link", "invite", "revoke"); len(got) > 0 {
		t.Fatalf("a migration with a current PAT must not mint, register, link or invite: %q", got)
	}
	if tok, _ := e.st.LoadControllerPAT(); tok != "scion_pat_old" {
		t.Fatal("the controller PAT on disk changed")
	}
	rec := e.record()
	if rec.Mode != state.HubModeMigrated || rec.ControllerID != devUserID || rec.AdminID != admin.ID ||
		rec.ProjectID != "proj-1" || rec.HubMarker != "role-ctl-old" {
		t.Fatalf("record = %+v", rec)
	}
	if !hiAnyContains(rec.Repairs, "R2") || !hiAnyContains(e.logs, "migrated") {
		t.Fatalf("repairs %q, logs %q", rec.Repairs, e.logs)
	}

	// Nothing changed since: no rename, no demotion, so no probe.
	n := len(h.events)
	e.mustRun()
	if got := hiEventsWith(h.eventsFrom(n), "patch", "identity", "agents by", "mint"); len(got) > 0 {
		t.Fatalf("second run: %q", got)
	}
}

func TestHubIdentityMigrationProbeFailureRestoresTheAdmin(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.seedMigrated("scion@localhost", false)
	e.hub.agents[1].Actions = []string{"read", "start", "stop", "message", "delete"}

	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "agent worker: attach") || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("run = %v, want the missing attach on worker and the restore", err)
	}
	if dev := e.hub.user(devUserID); dev.Role != "admin" {
		t.Fatalf("dev row role %q, want admin restored", dev.Role)
	}
	hiWantOrder(t, e.hub, "patch "+devUserID+" role=member status=", "agents by "+devUserID, "patch "+devUserID+" role=admin status=")
}

func TestHubIdentityRepairsADevUserRenamedToTheAdmin(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.seedMigrated(hubauth.AdminEmail, false)
	e.hub.staleAfterRename = 2
	e.mustRun()
	h := e.hub

	// The repair goes with the PAT, which belongs to the dev user.
	admin := h.byEmail(hubauth.AdminEmail)
	if admin == nil || admin.ID == devUserID || admin.Role != "admin" {
		t.Fatalf("admin row = %+v", admin)
	}
	hiWantOrder(t, h, "identity "+hubauth.ControllerEmail+" by "+devUserID, "admin row "+admin.ID, "patch "+devUserID+" role=member status=")
	if len(e.sleeps) != 2 {
		t.Fatalf("waited %v, want two polls while the hub's cache is stale", e.sleeps)
	}
	if dev := h.user(devUserID); dev.Role != "member" || dev.Email != hubauth.ControllerEmail {
		t.Fatalf("dev row = %+v", dev)
	}
	if rec := e.record(); !hiAnyContains(rec.Repairs, "R1") || rec.AdminID != admin.ID {
		t.Fatalf("record = %+v", rec)
	}
}

func TestHubIdentityR1GivesUpWhenTheCacheStaysStale(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.seedMigrated(hubauth.AdminEmail, false)
	e.hub.staleAfterRename = 100
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "still resolves") {
		t.Fatalf("run = %v", err)
	}
	if e.hub.at("patch "+devUserID+" role=member status=") >= 0 {
		t.Fatal("nothing may be demoted while lever-admin resolves to the dev user")
	}
}

func TestHubIdentityR1IsRefusedOnAFreshHub(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	if err := e.st.SaveHubIdentity(state.HubIdentity{Version: state.HubIdentityVersion, Mode: state.HubModeFresh,
		AdminID: "user-old-admin", ControllerID: "user-old-ctl", ProjectID: hiMarkerID, HubMarker: "role-x"}); err != nil {
		t.Fatal(err)
	}
	e.hub.addUser(devUserID, hubauth.AdminEmail, "admin", "active", e.now.Add(-time.Hour))
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "dev auth") {
		t.Fatalf("run = %v, want the dev-auth refusal", err)
	}
	if got := hiEventsWith(e.hub.events, "identity", "patch"); len(got) > 0 {
		t.Fatalf("a fresh hub must not be repaired: %q", got)
	}
}

func TestHubIdentityRebootstrapsANewHubDatabase(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(true, hiOperator("op@example.com")))
	e.mustRun()
	old := e.record()
	oldTok, _ := e.st.LoadControllerPAT()

	h := e.newHub()
	e.now = e.now.Add(48 * time.Hour)
	e.mustRun()

	if !hiAnyContains(e.logs, "WARNING", "bootstrapping it as new") {
		t.Fatalf("no loud re-bootstrap log: %q", e.logs)
	}
	rec := e.record()
	ctl := h.byEmail(hubauth.ControllerEmail)
	if rec.Mode != state.HubModeFresh || rec.ControllerID != ctl.ID || rec.HubMarker == old.HubMarker || rec.HubMarker == "" {
		t.Fatalf("record = %+v (old %+v)", rec, old)
	}
	tok := hiTokenNamed(h, "lever-controller", ctl.ID)
	if tok == nil || !e.diskPATIs(e.st.LoadControllerPAT, tok.ID) {
		t.Fatal("no new controller PAT on disk")
	}
	if now, _ := e.st.LoadControllerPAT(); now == oldTok {
		t.Fatal("the old hub's PAT is still on disk")
	}
	if h.at("scion link "+hiJailMount) < 0 || h.at("register "+hiMarkerID+" by "+ctl.ID) < 0 {
		t.Fatalf("the new hub was not bootstrapped: %q", h.events)
	}
	if u := h.byEmail("op@example.com"); u == nil || rec.Users["op@example.com"] != u.ID {
		t.Fatalf("the remote login was not re-invited: %+v", rec.Users)
	}
}

func TestHubIdentityMarkerGoneWithAnOldAdminFailsClosed(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.mustRun()
	old := e.record()

	h := e.newHub()
	h.addUser("", hubauth.AdminEmail, "admin", "active", e.now.Add(-time.Hour))
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "--reset-hub-identity") {
		t.Fatalf("run = %v, want the reset hint", err)
	}
	if rec := e.record(); rec.ControllerID != old.ControllerID || rec.HubMarker != old.HubMarker {
		t.Fatalf("the records changed: %+v", rec)
	}
	if tok, _ := e.st.LoadControllerPAT(); tok == "" {
		t.Fatal("the PAT was forgotten")
	}
	if got := hiEventsWith(h.events, "invite", "mint", "register", "patch"); len(got) > 0 {
		t.Fatalf("the hub was changed: %q", got)
	}
}

func TestHubIdentityUnansweredRoleListIsNotEvidence(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.mustRun()
	old := e.record()

	h := e.newHub()
	h.down["GET /api/v1/admin/roles"] = http.StatusNotFound
	if err := e.run(); err == nil {
		t.Fatal("a 404 on the role list must fail the step")
	}
	if rec := e.record(); rec.ControllerID != old.ControllerID {
		t.Fatalf("the records changed: %+v", rec)
	}
}

func TestHubIdentityResetForgetsAndBootstraps(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.mustRun()
	old := e.record()

	h := e.newHub()
	h.addUser("", hubauth.AdminEmail, "admin", "active", e.now.Add(-time.Hour))
	e.reset = true
	e.mustRun()
	if !hiAnyContains(e.logs, "--reset-hub-identity") {
		t.Fatalf("logs %q", e.logs)
	}
	rec := e.record()
	ctl := h.byEmail(hubauth.ControllerEmail)
	if rec.Mode != state.HubModeFresh || rec.ControllerID != ctl.ID || rec.ControllerID == old.ControllerID {
		t.Fatalf("record = %+v", rec)
	}
	if tok := hiTokenNamed(h, "lever-controller", ctl.ID); tok == nil || !e.diskPATIs(e.st.LoadControllerPAT, tok.ID) {
		t.Fatal("no new controller PAT")
	}
}

func TestHubIdentityRemoteLoginOnTheDevRowIsRefused(t *testing.T) {
	op, contact := "op@example.com", "c1@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	e.hub.addUser(devUserID, op, "member", "active", e.now.Add(-time.Hour))
	e.mustRun()

	if !hiAnyContains(e.warns, op, "refused") {
		t.Fatalf("warnings %q", e.warns)
	}
	if got := e.hub.boundTo(devUserID); len(got) != 0 || e.hub.ceilingOn(devUserID) != nil {
		t.Fatalf("the dev row was bound %q", got)
	}
	rec, rr := e.record(), e.remoteRole()
	c := e.hub.byEmail(contact)
	if _, ok := rec.Users[op]; ok || rec.Users[contact] != c.ID || rr.Bound[op] != "" || rr.Bound[contact] != c.ID {
		t.Fatalf("users %v, bound %v", rec.Users, rr.Bound)
	}
	if !slices.Equal(e.hub.boundTo(c.ID), []string{contactRoleName}) {
		t.Fatalf("contact bound %q", e.hub.boundTo(c.ID))
	}
}

func TestHubIdentityRemoteUserIDRefusesLeverRows(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(true))
	u := e.hub.addUser("", "x@example.com", "member", "active", e.now)
	for name, rec := range map[string]state.HubIdentity{
		"controller": {ControllerID: u.ID},
		"admin":      {AdminID: u.ID},
	} {
		if _, err := e.step().remoteUserID(context.Background(), "x@example.com", &rec); !errors.Is(err, errLoginRefused) {
			t.Errorf("%s: err = %v, want errLoginRefused", name, err)
		}
	}
	for _, email := range []string{hubauth.ControllerEmail, "Lever-Admin@Lever.Local", "scion@localhost"} {
		if _, err := e.step().remoteUserID(context.Background(), email, &state.HubIdentity{}); !errors.Is(err, errLoginRefused) {
			t.Errorf("%s: err = %v, want errLoginRefused", email, err)
		}
	}
	if got := hiEventsWith(e.hub.events, "invite"); len(got) > 0 {
		t.Fatalf("a reserved address was invited: %q", got)
	}
}

func TestHubIdentityRemoteLoginWithANewRowIsRefused(t *testing.T) {
	op, other := "op@example.com", "op2@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiOperator(other)))
	e.mustRun()
	oldID := e.record().Users[op]

	e.hub.removeUser(oldID)
	fresh := e.hub.addUser("", op, "member", "active", e.now)
	e.mustRun()
	if !hiAnyContains(e.warns, op, oldID) {
		t.Fatalf("warnings %q", e.warns)
	}
	if got := e.hub.boundTo(fresh.ID); len(got) != 0 {
		t.Fatalf("the new row was bound %q", got)
	}
	rec, rr := e.record(), e.remoteRole()
	if rec.Users[op] != oldID || rr.Bound[op] != "" || rr.Bound[other] == "" {
		t.Fatalf("users %v, bound %v", rec.Users, rr.Bound)
	}
}

func TestHubIdentityRemovedLoginIsSuspendedThenReactivated(t *testing.T) {
	op, contact := "op@example.com", "c1@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	e.mustRun()
	c := e.hub.byEmail(contact)

	e.app.Remote.AllowedUsers = []config.RemoteUser{hiOperator(op)}
	e.mustRun()
	hiWantOrder(t, e.hub, "patch "+c.ID+" role= status=suspended", "unbind "+contactRoleName+" "+c.ID)
	if c.Status != "suspended" || len(e.hub.boundTo(c.ID)) != 0 {
		t.Fatalf("removed contact: %+v bound %q", c, e.hub.boundTo(c.ID))
	}
	if rr := e.remoteRole(); rr.Bound[contact] != "" {
		t.Fatalf("remote-role.json still binds the removed login: %v", rr.Bound)
	}

	n := len(e.hub.events)
	e.mustRun()
	if got := hiEventsWith(e.hub.eventsFrom(n), "patch"); len(got) > 0 {
		t.Fatalf("an already suspended login was patched again: %q", got)
	}

	e.app.Remote.AllowedUsers = []config.RemoteUser{hiOperator(op), hiContact(contact)}
	e.mustRun()
	hiWantOrder(t, e.hub, "patch "+c.ID+" role= status=active", "bind "+contactRoleName+" "+c.ID)
	if c.Status != "active" || !slices.Equal(e.hub.boundTo(c.ID), []string{contactRoleName}) {
		t.Fatalf("re-added contact: %+v bound %q", c, e.hub.boundTo(c.ID))
	}
}

func TestHubIdentityTierMoveCapsFirst(t *testing.T) {
	op := "op@example.com"
	e := newHIEnv(t, hiRemoteApp(true, hiOperator(op)))
	e.mustRun()
	u := e.hub.byEmail(op)
	web := strings.Join(e.hub.ceilingOn(u.ID).Perms, ",")
	contactPerms := strings.Join(contactRolePermissions(), ",")

	e.app.Remote.AllowedUsers = []config.RemoteUser{hiContact(op)}
	n := len(e.hub.events)
	e.mustRun()
	got := hiEventsWith(e.hub.eventsFrom(n), "ceiling", "bind", "unbind")
	want := []string{
		"ceiling+ " + u.ID + " " + contactPerms,
		"ceiling- " + u.ID + " " + web,
		"unbind " + remoteWebRoleName + " " + u.ID,
		"bind " + contactRoleName + " " + u.ID,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("operator to contact: %q, want %q", got, want)
	}

	e.app.Remote.AllowedUsers = []config.RemoteUser{hiOperator(op)}
	n = len(e.hub.events)
	e.mustRun()
	got = hiEventsWith(e.hub.eventsFrom(n), "ceiling", "bind", "unbind")
	want = []string{
		"ceiling+ " + u.ID + " " + web,
		"ceiling- " + u.ID + " " + contactPerms,
		"unbind " + contactRoleName + " " + u.ID,
		"bind " + remoteWebRoleName + " " + u.ID,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("contact to operator: %q, want %q", got, want)
	}
	if rr := e.remoteRole(); len(rr.Contacts) != 0 || rr.Bound[op] != u.ID {
		t.Fatalf("remote-role.json = %+v", rr)
	}
}

func TestHubIdentityControllerRowChecks(t *testing.T) {
	t.Run("admin", func(t *testing.T) {
		e := newHIEnv(t, hiRemoteApp(false))
		e.hub.addUser("", hubauth.ControllerEmail, "admin", "active", e.now)
		rec := state.HubIdentity{}
		if _, _, err := e.step().ensureController(context.Background(), state.HubModeFresh, &rec); err == nil ||
			!strings.Contains(err.Error(), "want an active member") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("suspended", func(t *testing.T) {
		e := newHIEnv(t, hiRemoteApp(false))
		e.hub.resolveSuspended = true
		e.hub.addUser("", hubauth.ControllerEmail, "member", "suspended", e.now)
		rec := state.HubIdentity{}
		if _, _, err := e.step().ensureController(context.Background(), state.HubModeFresh, &rec); err == nil ||
			!strings.Contains(err.Error(), "want an active member") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not the recorded controller", func(t *testing.T) {
		e := newHIEnv(t, hiRemoteApp(false))
		e.hub.addUser("", hubauth.ControllerEmail, "member", "active", e.now)
		rec := state.HubIdentity{ControllerID: "user-recorded"}
		if _, _, err := e.step().ensureController(context.Background(), state.HubModeFresh, &rec); err == nil ||
			!strings.Contains(err.Error(), "not the recorded controller") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an admin the hub will not demote", func(t *testing.T) {
		e := newHIEnv(t, hiRemoteApp(false))
		x := e.hub.addUser("", "x@example.com", "admin", "active", e.now)
		e.hub.keepRole[x.ID] = true
		if err := e.run(); err == nil || !strings.Contains(err.Error(), "still a hub admin") {
			t.Fatalf("run = %v", err)
		}
		if got := hiEventsWith(e.hub.events, "invite", "register", "mint"); len(got) > 0 {
			t.Fatalf("the step went on: %q", got)
		}
	})
}

func TestHubIdentityMigratedControllerAddressTaken(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(false))
	e.seedMigrated("scion@localhost", false)
	e.hub.addUser("", hubauth.ControllerEmail, "member", "active", e.now.Add(-time.Hour))
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "another hub user holds the address") {
		t.Fatalf("run = %v", err)
	}
	if e.hub.at("identity "+hubauth.ControllerEmail+" ignored by "+e.hub.byEmail(hubauth.AdminEmail).ID) < 0 {
		t.Fatalf("events %q", e.hub.events)
	}
	if got := hiEventsWith(e.hub.events, "mint", "bind"); len(got) > 0 {
		t.Fatalf("the step went on: %q", got)
	}
}

func TestHubIdentityRemoteUsersFor(t *testing.T) {
	if got := remoteUsersFor(hiRemoteApp(false)); got != nil {
		t.Fatalf("remote off, no logins: %v", got)
	}
	// Invited while remote access is off, so an agent cannot claim the address
	// first.
	got := remoteUsersFor(hiRemoteApp(false, hiOperator("Op@Example.com"), hiContact("c1@example.com")))
	want := []remoteUser{{Email: "op@example.com"}, {Email: "c1@example.com", Contact: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("remote off: %v, want %v", got, want)
	}
	if got := remoteUsersFor(hiRemoteApp(true)); len(got) != 1 || got[0].Contact || got[0].Email != config.HubEmailFor("") {
		t.Fatalf("remote on, no logins: %v", got)
	}
}

// TestHubIdentityProbeRunsAfterAnInterruptedMigration: a migration that
// demoted and renamed the controller but failed before its probe leaves the
// probe pending, and the next run probes even though it changes nothing.
func TestHubIdentityProbeRunsAfterAnInterruptedMigration(t *testing.T) {
	e := newHIEnv(t, hiRemoteApp(true))
	e.seedMigrated("scion@localhost", false)
	e.hub.down = map[string]int{"POST /api/v1/admin/access-constraint-previews": 500}
	if err := e.run(); err == nil {
		t.Fatal("the first run must fail at the remote logins")
	}
	if rec := e.record(); !rec.ProbePending {
		t.Fatalf("record = %+v, want the probe pending", rec)
	}
	if dev := e.hub.user(devUserID); dev.Role != "member" || dev.Email != hubauth.ControllerEmail {
		t.Fatalf("dev row = %+v", dev)
	}
	e.hub.down = nil
	e.hub.agents[1].Actions = []string{"read", "start", "stop", "message", "delete"}
	n := len(e.hub.events)
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "agent worker: attach") {
		t.Fatalf("second run = %v, want the pending probe to find the missing attach", err)
	}
	if got := hiEventsWith(e.hub.eventsFrom(n), "agents by "+devUserID); len(got) == 0 {
		t.Fatalf("the second run did not probe: %q", e.hub.eventsFrom(n))
	}
	if dev := e.hub.user(devUserID); dev.Role != "admin" {
		t.Fatalf("dev row role %q, want admin restored", dev.Role)
	}
}
