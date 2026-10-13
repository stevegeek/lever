package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func decodeBody(t *testing.T, f *fakeDoer, key string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(f.bodies[key]), &got); err != nil {
		t.Fatalf("body of %s: %v (%q)", key, err, f.bodies[key])
	}
	return got
}

func TestInviteUserDecodesTheInvitedRow(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/admin/users/invite": {status: 201,
		body: `{"id":"u-1","email":"c@example.com","status":"invited","invitedBy":"a-1","created":"2026-10-13T17:00:00Z"}`}}}
	u, err := (&Client{T: f}).InviteUser(context.Background(), "c@example.com")
	if err != nil {
		t.Fatalf("InviteUser: %v", err)
	}
	want := UserRecord{ID: "u-1", Email: "c@example.com", Status: "invited", InvitedBy: "a-1",
		Created: time.Date(2026, 10, 13, 17, 0, 0, 0, time.UTC)}
	if u != want {
		t.Fatalf("got %+v, want %+v", u, want)
	}
	if got := decodeBody(t, f, "POST /api/v1/admin/users/invite"); !reflect.DeepEqual(got, map[string]any{"email": "c@example.com"}) {
		t.Fatalf("body = %v", got)
	}
}

func TestInviteUserConflictIsErrUserExists(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/admin/users/invite": {status: 409, body: `{"error":{"code":"conflict","message":"user already exists"}}`}}}
	_, err := (&Client{T: f}).InviteUser(context.Background(), "c@example.com")
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("err = %v, want ErrUserExists", err)
	}
}

func TestInviteUserOtherFailuresAreAPIErrors(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/admin/users/invite": {status: 403, body: `{"error":"forbidden"}`}}}
	_, err := (&Client{T: f}).InviteUser(context.Background(), "c@example.com")
	assertAPIStatus(t, err, 403)
	if errors.Is(err, ErrUserExists) {
		t.Fatal("a 403 is not ErrUserExists")
	}
	f = &fakeDoer{replies: map[string]reply{"POST /api/v1/admin/users/invite": {status: 201, body: `{}`}}}
	if _, err := (&Client{T: f}).InviteUser(context.Background(), "c@example.com"); err == nil {
		t.Fatal("an invite answer without an id must fail")
	}
}

func TestGetUser(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/users/u-1":  {status: 200, body: `{"id":"u-1","email":"a@b","role":"member","status":"active","_capabilities":{}}`},
		"GET /api/v1/users/gone": {status: 404, body: `{"error":"not found"}`},
		"GET /api/v1/users/bad":  {status: 500, body: `oops`},
		"GET /api/v1/users/odd":  {status: 200, body: `{"id":"other"}`},
	}}
	c := &Client{T: f}
	u, found, err := c.GetUser(context.Background(), "u-1")
	if err != nil || !found || u.Role != "member" || u.Status != "active" || u.Email != "a@b" {
		t.Fatalf("got %+v found=%v err=%v", u, found, err)
	}
	if _, found, err := c.GetUser(context.Background(), "gone"); err != nil || found {
		t.Fatalf("404: found=%v err=%v, want not found and no error", found, err)
	}
	_, _, err = c.GetUser(context.Background(), "bad")
	assertAPIStatus(t, err, 500)
	if _, _, err := c.GetUser(context.Background(), "odd"); err == nil {
		t.Fatal("a row for another id must fail")
	}
}

func TestUpdateUserOmitsEmptyFields(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"PATCH /api/v1/users/u-1": {status: 200, body: `{"id":"u-1","role":"member","status":"active"}`}}}
	c := &Client{T: f}
	u, err := c.UpdateUser(context.Background(), "u-1", "member", "")
	if err != nil || u.Role != "member" {
		t.Fatalf("got %+v err=%v", u, err)
	}
	if got := decodeBody(t, f, "PATCH /api/v1/users/u-1"); !reflect.DeepEqual(got, map[string]any{"role": "member"}) {
		t.Fatalf("body = %v", got)
	}
	if _, err := c.UpdateUser(context.Background(), "u-1", "", "suspended"); err != nil {
		t.Fatal(err)
	}
	if got := decodeBody(t, f, "PATCH /api/v1/users/u-1"); !reflect.DeepEqual(got, map[string]any{"status": "suspended"}) {
		t.Fatalf("body = %v", got)
	}
	n := len(f.calls)
	if _, err := c.UpdateUser(context.Background(), "u-1", "", ""); err == nil || len(f.calls) != n {
		t.Fatalf("an empty update must fail without a request; err=%v", err)
	}
}

// The hub refuses a role change on a user who has not signed in yet.
func TestUpdateUserConflictKeepsItsStatus(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"PATCH /api/v1/users/u-1": {status: 409, body: `{"error":"role is assigned at first sign-in"}`}}}
	_, err := (&Client{T: f}).UpdateUser(context.Background(), "u-1", "member", "")
	assertAPIStatus(t, err, 409)
}

func TestUserByEmailFollowsPages(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/users?limit=200&search=a%40b":            {status: 200, body: `{"users":[{"id":"u-x","email":"xa@b"}],"nextCursor":"200"}`},
		"GET /api/v1/users?cursor=200&limit=200&search=a%40b": {status: 200, body: `{"users":[{"id":"u-a","email":"A@b","status":"invited"}]}`},
	}}
	u, found, err := (&Client{T: f}).UserByEmail(context.Background(), "a@b")
	if err != nil || !found || u.ID != "u-a" || u.Status != "invited" {
		t.Fatalf("got %+v found=%v err=%v", u, found, err)
	}
}

func TestUserByEmailRefusesAmbiguityAndBadShapes(t *testing.T) {
	two := &fakeDoer{replies: map[string]reply{"GET /api/v1/users?limit=200&search=a%40b": {status: 200,
		body: `{"users":[{"id":"1","email":"a@b"},{"id":"2","email":"A@B"}]}`}}}
	if _, _, err := (&Client{T: two}).UserByEmail(context.Background(), "a@b"); err == nil {
		t.Fatal("two exact matches must fail")
	}
	renamed := &fakeDoer{replies: map[string]reply{"GET /api/v1/users?limit=200&search=a%40b": {status: 200, body: `{"items":[]}`}}}
	if _, _, err := (&Client{T: renamed}).UserByEmail(context.Background(), "a@b"); err == nil {
		t.Fatal("a list without users must fail, not read as absent")
	}
	loop := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/users?limit=200&search=a%40b":          {status: 200, body: `{"users":[],"nextCursor":"1"}`},
		"GET /api/v1/users?cursor=1&limit=200&search=a%40b": {status: 200, body: `{"users":[],"nextCursor":"1"}`},
	}}
	if _, _, err := (&Client{T: loop}).UserByEmail(context.Background(), "a@b"); err == nil {
		t.Fatal("a repeating cursor must fail")
	}
	if _, _, err := (&Client{T: &fakeDoer{}}).UserByEmail(context.Background(), ""); err == nil {
		t.Fatal("an empty email must fail")
	}
}

const (
	rolesBody = `{"items":[
	  {"id":"r-sa","name":"super-admin","scopeType":"system","system":true},
	  {"id":"r-po","name":"project-owner","scopeType":"project","system":true}]}`
	bindingsKey0 = "GET /api/v1/admin/role-bindings?limit=200&offset=0&sort_by=created&sort_order=asc"
	bindingsKey1 = "GET /api/v1/admin/role-bindings?limit=200&offset=2&sort_by=created&sort_order=asc"
	adminUsers   = "GET /api/v1/users?limit=200&role=admin"
)

// Both signals count, the server's filters are re-checked, and pages are
// followed to the end.
func TestListAdminsUnionsRoleAndBinding(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		adminUsers: {status: 200, body: `{"users":[
		  {"id":"u-admin","email":"lever-admin@lever.local","role":"admin"},
		  {"id":"u-both","email":"both@x","role":"admin"},
		  {"id":"u-member","email":"m@x","role":"member"}],"nextCursor":"200"}`},
		"GET /api/v1/users?cursor=200&limit=200&role=admin": {status: 200, body: `{"users":[{"id":"u-late","email":"late@x","role":"admin"}]}`},
		"GET /api/v1/admin/roles":                           {status: 200, body: rolesBody},
		bindingsKey0: {status: 200, body: `{"items":[
		  {"id":"b-1","roleDefinitionId":"r-sa","principalType":"user","principalId":"u-dev","scopeType":"system","createdBy":"system-reconcile"},
		  {"id":"b-2","roleDefinitionId":"r-po","principalType":"user","principalId":"u-member","scopeType":"project","scopeId":"p"}],
		  "totalCount":3}`},
		bindingsKey1: {status: 200, body: `{"items":[
		  {"id":"b-3","roleDefinitionId":"r-sa","principalType":"user","principalId":"u-both","scopeType":"system","createdBy":"u-admin"}],
		  "totalCount":3}`},
		"GET /api/v1/users/u-dev": {status: 200, body: `{"id":"u-dev","email":"scion@localhost","role":"member"}`},
	}}
	got, err := (&Client{T: f}).ListAdmins(context.Background())
	if err != nil {
		t.Fatalf("ListAdmins: %v", err)
	}
	want := []AdminEntry{
		{UserID: "u-admin", Email: "lever-admin@lever.local", ByRole: true},
		{UserID: "u-both", Email: "both@x", ByRole: true, BindingIDs: []string{"b-3"}, BindingCreatedBy: []string{"u-admin"}},
		{UserID: "u-dev", Email: "scion@localhost", BindingIDs: []string{"b-1"}, BindingCreatedBy: []string{"system-reconcile"}},
		{UserID: "u-late", Email: "late@x", ByRole: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestListAdminsFailsClosed(t *testing.T) {
	base := func() map[string]reply {
		return map[string]reply{
			adminUsers:                {status: 200, body: `{"users":[]}`},
			"GET /api/v1/admin/roles": {status: 200, body: rolesBody},
			bindingsKey0:              {status: 200, body: `{"items":[],"totalCount":0}`},
		}
	}
	cases := map[string]func(m map[string]reply){
		"no super-admin role": func(m map[string]reply) {
			m["GET /api/v1/admin/roles"] = reply{status: 200, body: `{"items":[{"id":"r-po","name":"project-owner"}]}`}
		},
		"group holds super-admin": func(m map[string]reply) {
			m[bindingsKey0] = reply{status: 200, body: `{"items":[{"id":"b","roleDefinitionId":"r-sa","principalType":"group","principalId":"g"}],"totalCount":1}`}
		},
		"binding list short": func(m map[string]reply) {
			m[bindingsKey0] = reply{status: 200, body: `{"items":[],"totalCount":5}`}
		},
		"binding list renamed": func(m map[string]reply) {
			m[bindingsKey0] = reply{status: 200, body: `{"bindings":[]}`}
		},
		"user list renamed": func(m map[string]reply) {
			m[adminUsers] = reply{status: 200, body: `{"items":[]}`}
		},
		"user list error": func(m map[string]reply) {
			m[adminUsers] = reply{status: 403, body: `{}`}
		},
	}
	for name, mutate := range cases {
		m := base()
		mutate(m)
		_, err := (&Client{T: &fakeDoer{replies: m}}).ListAdmins(context.Background())
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
		if name == "group holds super-admin" && !errors.Is(err, ErrUnlistableAdmin) {
			t.Errorf("%s: want ErrUnlistableAdmin, got %v", name, err)
		}
	}
	if _, err := (&Client{T: &fakeDoer{replies: base()}}).ListAdmins(context.Background()); err != nil {
		t.Fatalf("an empty hub is not an error: %v", err)
	}
}

func TestSetDevIdentity(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"PUT /api/v1/system/identity": {status: 200, body: `{"email":"x"}`}}}
	c := &Client{T: f}
	if err := c.SetDevIdentity(context.Background(), "lever-controller@lever.local"); err != nil {
		t.Fatal(err)
	}
	if got := decodeBody(t, f, "PUT /api/v1/system/identity"); !reflect.DeepEqual(got, map[string]any{"email": "lever-controller@lever.local"}) {
		t.Fatalf("body = %v", got)
	}
	if err := c.SetDevIdentity(context.Background(), ""); err == nil {
		t.Fatal("an empty email must fail")
	}
	f.replies["PUT /api/v1/system/identity"] = reply{status: 403, body: `{"error":"loopback only"}`}
	err := c.SetDevIdentity(context.Background(), "x@y")
	assertAPIStatus(t, err, 403)
	if !strings.Contains(err.Error(), "loopback only") {
		t.Fatalf("error should carry the hub's reason: %v", err)
	}
}
