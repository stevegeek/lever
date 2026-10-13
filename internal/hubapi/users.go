package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

// SystemRoleSuperAdmin is the name of scion's hub-wide admin role.
const SystemRoleSuperAdmin = "super-admin"

// ErrUserExists is InviteUser's answer when the hub already holds a user with
// that email (HTTP 409), invited or signed in.
var ErrUserExists = errors.New("hub user already exists")

// ErrNotFound means the hub answered 404 for the object named.
var ErrNotFound = errors.New("not found on the hub")

// ErrUnlistableAdmin means a non-user principal (a group or an agent) holds
// the super-admin role, so the set of admins cannot be enumerated as users.
var ErrUnlistableAdmin = errors.New("a non-user principal holds the hub super-admin role")

// listPageLimit bounds every paged walk: a hub that keeps handing out pages
// must not keep lever looping, and a partial admin list must not pass as
// complete.
const listPageLimit = 200

// InviteUser pre-provisions a user by email (POST /api/v1/admin/users/invite)
// so bindings and ceilings can be attached before the first sign-in. A user
// the hub already holds comes back as ErrUserExists.
func (c *Client) InviteUser(ctx context.Context, email string) (UserRecord, error) {
	var u UserRecord
	err := c.send(ctx, http.MethodPost, "/api/v1/admin/users/invite", struct {
		Email string `json:"email"`
	}{email}, &u)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return UserRecord{}, fmt.Errorf("%w: %s", ErrUserExists, ae.Msg)
	}
	if err != nil {
		return UserRecord{}, err
	}
	if u.ID == "" {
		return UserRecord{}, &APIError{Msg: "POST /api/v1/admin/users/invite returned no user id"}
	}
	return u, nil
}

// GetUser reads one user by ID (GET /api/v1/users/{id}); found is false on 404.
func (c *Client) GetUser(ctx context.Context, id string) (UserRecord, bool, error) {
	var u UserRecord
	err := c.get(ctx, "/api/v1/users/"+url.PathEscape(id), &u)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return UserRecord{}, false, nil
	}
	if err != nil {
		return UserRecord{}, false, err
	}
	if u.ID != id {
		return UserRecord{}, false, &APIError{Msg: fmt.Sprintf("GET user %s returned user %q", id, u.ID)}
	}
	return u, true, nil
}

// UpdateUser changes a user's role and/or status (PATCH /api/v1/users/{id});
// an empty argument is left out of the request. The hub answers 409 for a
// role change on a user still invited, surfaced as an APIError with Status
// 409.
func (c *Client) UpdateUser(ctx context.Context, id, role, status string) (UserRecord, error) {
	if role == "" && status == "" {
		return UserRecord{}, fmt.Errorf("hubapi: updating user %s: nothing to change", id)
	}
	var u UserRecord
	err := c.send(ctx, http.MethodPatch, "/api/v1/users/"+url.PathEscape(id), struct {
		Role   string `json:"role,omitempty"`
		Status string `json:"status,omitempty"`
	}{role, status}, &u)
	return u, err
}

// AdminEntry is one hub admin and how it is one: by the user role, by
// bindings to the super-admin role, or both.
type AdminEntry struct {
	UserID           string
	Email            string
	ByRole           bool
	BindingIDs       []string
	BindingCreatedBy []string
}

// ListAdmins lists every user that is a hub admin by either signal scion
// grants on: the user role admin, or a binding to the super-admin system role.
// Server-side filters only narrow the read; membership is decided here. A
// group or agent bound to super-admin is ErrUnlistableAdmin, since its members
// would be admins this list cannot name.
func (c *Client) ListAdmins(ctx context.Context) ([]AdminEntry, error) {
	byID := map[string]*AdminEntry{}
	entry := func(id string) *AdminEntry {
		e, ok := byID[id]
		if !ok {
			e = &AdminEntry{UserID: id}
			byID[id] = e
		}
		return e
	}

	users, err := c.listUsers(ctx, url.Values{"role": {"admin"}},
		func(u UserRecord) bool { return u.Role == "admin" })
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		e := entry(u.ID)
		e.Email, e.ByRole = u.Email, true
	}

	defs, err := c.RoleDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	superAdmin := map[string]bool{}
	for _, d := range defs {
		if d.Name == SystemRoleSuperAdmin && d.ID != "" {
			superAdmin[d.ID] = true
		}
	}
	if len(superAdmin) == 0 {
		return nil, &APIError{Msg: fmt.Sprintf("the hub lists no %q role; cannot tell which users are admins", SystemRoleSuperAdmin)}
	}

	bindings, err := c.allRoleBindings(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if !superAdmin[b.RoleDefinitionID] {
			continue
		}
		if b.PrincipalType != PrincipalUser {
			return nil, fmt.Errorf("%w: %s %s (binding %s)", ErrUnlistableAdmin, b.PrincipalType, b.PrincipalID, b.ID)
		}
		e := entry(b.PrincipalID)
		e.BindingIDs = append(e.BindingIDs, b.ID)
		e.BindingCreatedBy = append(e.BindingCreatedBy, b.CreatedBy)
	}

	out := make([]AdminEntry, 0, len(byID))
	for _, e := range byID {
		if e.Email == "" {
			u, found, err := c.GetUser(ctx, e.UserID)
			if err != nil {
				return nil, err
			}
			if found {
				e.Email = u.Email
			}
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

// listUsers walks GET /api/v1/users with the given filter by its cursor and
// returns the rows keep accepts.
func (c *Client) listUsers(ctx context.Context, filter url.Values, keep func(UserRecord) bool) ([]UserRecord, error) {
	var out []UserRecord
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page == listPageLimit {
			return nil, &APIError{Msg: fmt.Sprintf("the hub's user list did not end after %d pages", listPageLimit)}
		}
		q := url.Values{"limit": {"200"}}
		for k, v := range filter {
			q[k] = v
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var raw map[string]json.RawMessage
		if err := c.get(ctx, "/api/v1/users?"+q.Encode(), &raw); err != nil {
			return nil, err
		}
		// Require the key: a renamed field would read as "no such user".
		field, ok := raw["users"]
		if !ok {
			return nil, &APIError{Msg: "user list has no \"users\" field; the hub API changed shape"}
		}
		var users []UserRecord
		if err := json.Unmarshal(field, &users); err != nil {
			return nil, &APIError{Msg: fmt.Sprintf("decoding user list: %v", err)}
		}
		for _, u := range users {
			if keep(u) {
				out = append(out, u)
			}
		}
		var next string
		if n, ok := raw["nextCursor"]; ok {
			if err := json.Unmarshal(n, &next); err != nil {
				return nil, &APIError{Msg: fmt.Sprintf("decoding user list cursor: %v", err)}
			}
		}
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, &APIError{Msg: "the hub's user list repeated a page cursor"}
		}
		seen[next] = true
		cursor = next
	}
}

// allRoleBindings walks the unfiltered admin binding list
// (GET /api/v1/admin/role-bindings) by offset, oldest first so a binding
// created during the walk lands after it rather than shifting a page.
func (c *Client) allRoleBindings(ctx context.Context) ([]RoleBinding, error) {
	const pageSize = 200
	var out []RoleBinding
	for page := 0; ; page++ {
		if page == listPageLimit {
			return nil, &APIError{Msg: fmt.Sprintf("the hub's role binding list did not end after %d pages", listPageLimit)}
		}
		q := url.Values{
			"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(len(out))},
			"sort_by": {"created"}, "sort_order": {"asc"},
		}
		var body struct {
			Items      *[]RoleBinding `json:"items"`
			TotalCount *int           `json:"totalCount"`
		}
		if err := c.get(ctx, "/api/v1/admin/role-bindings?"+q.Encode(), &body); err != nil {
			return nil, err
		}
		if body.Items == nil || body.TotalCount == nil {
			return nil, &APIError{Msg: "role binding list has no \"items\" or \"totalCount\" field; the hub API changed shape"}
		}
		out = append(out, *body.Items...)
		if len(*body.Items) == 0 || len(out) >= *body.TotalCount {
			if len(out) < *body.TotalCount {
				return nil, &APIError{Msg: fmt.Sprintf("the hub's role binding list ended at %d of %d bindings", len(out), *body.TotalCount)}
			}
			return out, nil
		}
	}
}

// SetDevIdentity sets the email of the hub's dev-identity user row
// (PUT /api/v1/system/identity). A 200 does not prove the row changed: the
// hub writes its settings file and then updates the row best effort (an email
// another user holds is dropped silently), so callers read the row back.
func (c *Client) SetDevIdentity(ctx context.Context, email string) error {
	if email == "" {
		return errors.New("hubapi: setting the dev identity: empty email")
	}
	return c.send(ctx, http.MethodPut, "/api/v1/system/identity", struct {
		Email string `json:"email"`
	}{email}, nil)
}
