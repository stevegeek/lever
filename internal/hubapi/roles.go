package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The calls below are hub-ADMIN calls: role definitions, role bindings and
// the user directory. scion refuses them to any UAT (role and binding
// administration is super-admin/hub-admin only, and UATs are denied on
// hub-level resources), so no lever token can make them. They exist for one
// caller: the bootstrap-token step's throwaway dev-auth window, where the dev
// identity is the super-admin (see ensureRemoteWebRole in internal/cli/host).
//
// Shapes are scion's pkg/hub/handlers_roles.go (createRoleDefinitionRequest,
// updateRoleDefinitionRequest, createRoleBindingRequest,
// listRoleDefinitionsResponse, listRoleBindingsResponse) and
// pkg/hub/handlers_users_core.go (ListUsersResponse).

// RoleScopeProject is scion's scope type for a role that is bound per project
// (store.RoleScopeProject).
const RoleScopeProject = "project"

// RoleScopeSystem is scion's scope type for a hub-wide role
// (store.RoleScopeSystem), such as the seeded hub-member role.
const RoleScopeSystem = "system"

// PrincipalUser is scion's principal type for a hub user
// (store.RoleBindingPrincipalUser).
const PrincipalUser = "user"

// RoleDefinition mirrors the fields of scion's store.RoleDefinition that
// lever reads or writes. Permissions are registry IDs ("agent.read"), not UAT
// scopes ("agent:read").
type RoleDefinition struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ScopeType   string   `json:"scopeType"`
	Permissions []string `json:"permissions"`
	System      bool     `json:"system,omitempty"`
}

// RoleBinding mirrors the fields of scion's store.RoleBinding that lever
// reads or writes.
type RoleBinding struct {
	ID               string `json:"id,omitempty"`
	RoleDefinitionID string `json:"roleDefinitionId"`
	PrincipalType    string `json:"principalType"`
	PrincipalID      string `json:"principalId"`
	ScopeType        string `json:"scopeType"`
	ScopeID          string `json:"scopeId"`
}

// User mirrors the fields of scion's store.User that lever reads.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// ErrBindingExists is CreateRoleBinding's answer when the hub already holds
// this exact binding (HTTP 409). Callers that want "bound" treat it as
// success.
var ErrBindingExists = errors.New("role binding already exists")

// send issues one request with a JSON body and decodes a 2xx reply into out
// (nil discards it). A non-2xx reply is an APIError carrying the status.
func (c *Client) send(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("hubapi: encoding %s %s: %w", method, path, err)
	}
	status, resp, err := c.doBody(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &APIError{Status: status,
			Msg: fmt.Sprintf("%s %s: HTTP %d: %s", method, path, status, snippet(resp))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp, out); err != nil {
		return &APIError{Msg: fmt.Sprintf("%s %s: decoding response: %v", method, path, err)}
	}
	return nil
}

// RoleDefinitions lists every role definition on the hub, system and custom
// (GET /api/v1/admin/roles).
func (c *Client) RoleDefinitions(ctx context.Context) ([]RoleDefinition, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/admin/roles", &raw); err != nil {
		return nil, err
	}
	// Require the key, for the reason SharedDirs does: a renamed field would
	// decode to "no roles" and the caller would create a duplicate.
	field, ok := raw["items"]
	if !ok {
		return nil, &APIError{Msg: "role list has no \"items\" field; the hub API changed shape"}
	}
	var defs []RoleDefinition
	if err := json.Unmarshal(field, &defs); err != nil {
		return nil, &APIError{Msg: fmt.Sprintf("decoding role list: %v", err)}
	}
	return defs, nil
}

// CreateRoleDefinition creates a custom role (POST /api/v1/admin/roles) and
// returns it as the hub stored it.
func (c *Client) CreateRoleDefinition(ctx context.Context, rd RoleDefinition) (RoleDefinition, error) {
	var got RoleDefinition
	err := c.send(ctx, http.MethodPost, "/api/v1/admin/roles", struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		ScopeType   string   `json:"scopeType"`
		Permissions []string `json:"permissions"`
	}{rd.Name, rd.Description, rd.ScopeType, rd.Permissions}, &got)
	return got, err
}

// UpdateRoleDefinition replaces a custom role's name, description and
// permission set (PUT /api/v1/admin/roles/:id). The scope type cannot change.
func (c *Client) UpdateRoleDefinition(ctx context.Context, rd RoleDefinition) (RoleDefinition, error) {
	var got RoleDefinition
	err := c.send(ctx, http.MethodPut, "/api/v1/admin/roles/"+url.PathEscape(rd.ID), struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}{rd.Name, rd.Description, rd.Permissions}, &got)
	return got, err
}

// UserByEmail finds the hub user whose email is exactly email (ignoring
// case). found is false when there is none — a user who has never signed in.
//
// The hub's user list has no exact-email filter, only `search`, a
// case-insensitive substring match on email and display name
// (store/entadapter user_store.go). So the search narrows and the exact match
// here decides: "a@b" must not bind "xa@b".
func (c *Client) UserByEmail(ctx context.Context, email string) (User, bool, error) {
	var body struct {
		Users []User `json:"users"`
	}
	if err := c.get(ctx, "/api/v1/users?search="+url.QueryEscape(email), &body); err != nil {
		return User{}, false, err
	}
	var matched []User
	for _, u := range body.Users {
		if strings.EqualFold(u.Email, email) {
			matched = append(matched, u)
		}
	}
	if len(matched) > 1 {
		return User{}, false, &APIError{Msg: fmt.Sprintf("the hub lists %d users with email %q; refusing to guess which one", len(matched), email)}
	}
	if len(matched) == 0 {
		return User{}, false, nil
	}
	return matched[0], true, nil
}

// UserRoleBindings lists the bindings held DIRECTLY by a user — not the ones
// inherited through a group (GET /api/v1/admin/role-bindings?principalType=
// user&principalId=<id>).
func (c *Client) UserRoleBindings(ctx context.Context, userID string) ([]RoleBinding, error) {
	q := url.Values{"principalType": {PrincipalUser}, "principalId": {userID}}
	var body struct {
		Items []RoleBinding `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/admin/role-bindings?"+q.Encode(), &body); err != nil {
		return nil, err
	}
	return body.Items, nil
}

// CreateRoleBinding binds a principal to a role at a scope
// (POST /api/v1/admin/role-bindings). A binding the hub already holds comes
// back as ErrBindingExists.
func (c *Client) CreateRoleBinding(ctx context.Context, rb RoleBinding) (RoleBinding, error) {
	var got RoleBinding
	err := c.send(ctx, http.MethodPost, "/api/v1/admin/role-bindings", struct {
		RoleDefinitionID string `json:"roleDefinitionId"`
		PrincipalType    string `json:"principalType"`
		PrincipalID      string `json:"principalId"`
		ScopeType        string `json:"scopeType"`
		ScopeID          string `json:"scopeId"`
	}{rb.RoleDefinitionID, rb.PrincipalType, rb.PrincipalID, rb.ScopeType, rb.ScopeID}, &got)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return RoleBinding{}, fmt.Errorf("%w: %s", ErrBindingExists, ae.Msg)
	}
	return got, err
}

// SamePermissions reports whether two permission lists hold the same set,
// ignoring order and duplicates.
func SamePermissions(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}
