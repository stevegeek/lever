package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// The user list only has a substring search, so the exact match must be
// lever's: "you@github" must not resolve to "notyou@github".
func TestUserByEmailMatchesExactlyIgnoringCase(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/users?search=you%40github": {status: 200, body: `{"users":[
		  {"id":"u-other","email":"notyou@github"},
		  {"id":"u-you","email":"You@GitHub"}],"totalCount":2}`},
	}}
	u, found, err := (&Client{T: f}).UserByEmail(context.Background(), "you@github")
	if err != nil || !found || u.ID != "u-you" {
		t.Fatalf("got %+v found=%v err=%v, want u-you", u, found, err)
	}
}

func TestUserByEmailAbsentIsNotAnError(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/users?search=you%40github": {status: 200, body: `{"users":[{"id":"u-other","email":"notyou@github"}]}`},
	}}
	if _, found, err := (&Client{T: f}).UserByEmail(context.Background(), "you@github"); err != nil || found {
		t.Fatalf("found=%v err=%v, want not found and no error", found, err)
	}
}

// A renamed list field must fail, not read as "no roles" (which would create
// a duplicate role).
func TestRoleDefinitionsRequiresItems(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"GET /api/v1/admin/roles": {status: 200, body: `{"roles":[]}`}}}
	if _, err := (&Client{T: f}).RoleDefinitions(context.Background()); err == nil {
		t.Fatal("want an error for a list without items")
	}
}

// The create body is scion's createRoleBindingRequest; a 409 is
// ErrBindingExists.
func TestCreateRoleBindingBodyAndConflict(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/admin/role-bindings": {status: 409, body: `{"error":"this role binding already exists"}`}}}
	_, err := (&Client{T: f}).CreateRoleBinding(context.Background(), RoleBinding{
		RoleDefinitionID: "r", PrincipalType: PrincipalUser, PrincipalID: "u", ScopeType: RoleScopeProject, ScopeID: "p",
	})
	if !errors.Is(err, ErrBindingExists) {
		t.Fatalf("err = %v, want ErrBindingExists", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(f.bodies["POST /api/v1/admin/role-bindings"]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"roleDefinitionId": "r", "principalType": "user", "principalId": "u", "scopeType": "project", "scopeId": "p"}
	if len(got) != len(want) {
		t.Fatalf("body = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("body = %v, want %v", got, want)
		}
	}
}

// PUT carries name, description and permissions only: scion's
// updateRoleDefinitionRequest has no scopeType.
func TestUpdateRoleDefinitionBody(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"PUT /api/v1/admin/roles/r-1": {status: 200, body: `{"id":"r-1","permissions":["agent.read"]}`}}}
	got, err := (&Client{T: f}).UpdateRoleDefinition(context.Background(), RoleDefinition{
		ID: "r-1", Name: "n", Description: "d", ScopeType: RoleScopeProject, Permissions: []string{"agent.read"},
	})
	if err != nil || got.ID != "r-1" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.bodies["PUT /api/v1/admin/roles/r-1"]), &body); err != nil {
		t.Fatal(err)
	}
	if _, has := body["scopeType"]; has || body["name"] != "n" {
		t.Fatalf("body = %v", body)
	}
}

func TestSamePermissions(t *testing.T) {
	if !SamePermissions([]string{"a", "b", "b"}, []string{"b", "a"}) {
		t.Fatal("order and duplicates must not matter")
	}
	if SamePermissions([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("a missing permission is a difference")
	}
}
