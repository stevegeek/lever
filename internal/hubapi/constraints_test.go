package hubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// These structs copy scion's request types field for field, decoded strictly
// as scion does: an unknown field is rejected.
type scionSubject struct {
	Kind          string `json:"kind"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
	GroupID       string `json:"groupId,omitempty"`
}

type scionScope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type scionCondition struct {
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type scionDraft struct {
	Name               string          `json:"name"`
	Purpose            string          `json:"purpose"`
	Subject            scionSubject    `json:"subject"`
	Scope              scionScope      `json:"scope"`
	MaximumPermissions []string        `json:"maximumPermissions"`
	AppliesWhen        *scionCondition `json:"appliesWhen,omitempty"`
}

type scionPreviewRequest struct {
	Operation    string      `json:"operation"`
	Draft        *scionDraft `json:"draft,omitempty"`
	ConstraintID string      `json:"constraintId,omitempty"`
	BaseRevision string      `json:"baseRevision,omitempty"`
}

type scionCreateRequest struct {
	scionDraft
	PreviewToken string `json:"previewToken"`
}

// scionUpdateRequest is scion's accessConstraintUpdateRequest, the same shape.
type scionUpdateRequest = scionCreateRequest

func strictDecode(t *testing.T, body string, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("scion would reject %s: %v", body, err)
	}
}

var testDraft = ConstraintDraft{
	Name: "lever-remote-ceiling:you@github", Purpose: "p",
	Subject:            ConstraintSubject{Kind: ConstraintSubjectPrincipal, PrincipalType: PrincipalUser, PrincipalID: "u1"},
	Scope:              ConstraintScope{Type: ConstraintScopeSystem},
	MaximumPermissions: []string{"agent.read", "user.read"},
}

func TestCreateAccessConstraintPreviewsThenCommits(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"POST /api/v1/admin/access-constraint-previews": {status: 200, body: `{"previewId":"p1","previewToken":"tok-1","completeness":{"complete":true}}`},
		"POST /api/v1/admin/access-constraints": {status: 201, body: `{"id":"ac-1","name":"lever-remote-ceiling:you@github",
			"subject":{"kind":"principal","principalType":"user","principalId":"u1"},"scope":{"type":"system"},
			"status":"active","revision":"1","maximumPermissions":[{"id":"agent.read","displayName":"x"},{"id":"user.read"}],"auditId":"a1"}`},
	}}
	got, err := (&Client{T: f}).CreateAccessConstraint(context.Background(), testDraft)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"POST /api/v1/admin/access-constraint-previews", "POST /api/v1/admin/access-constraints"}) {
		t.Fatalf("calls = %v", f.calls)
	}
	var prev scionPreviewRequest
	strictDecode(t, f.bodies["POST /api/v1/admin/access-constraint-previews"], &prev)
	var create scionCreateRequest
	strictDecode(t, f.bodies["POST /api/v1/admin/access-constraints"], &create)
	if prev.Operation != "create" || prev.Draft == nil || create.PreviewToken != "tok-1" {
		t.Fatalf("preview %+v, create token %q", prev, create.PreviewToken)
	}
	// The token is bound to the draft's hash, so the commit must repeat it.
	pb, _ := json.Marshal(prev.Draft)
	cb, _ := json.Marshal(create.scionDraft)
	if string(pb) != string(cb) {
		t.Fatalf("commit draft %s differs from the previewed %s", cb, pb)
	}
	if got.ID != "ac-1" || got.ScopeType != "system" || got.Subject.PrincipalID != "u1" ||
		!slices.Equal(got.MaximumPermissions, []string{"agent.read", "user.read"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestCreateAccessConstraintBlockedPreviewCommitsNothing(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"POST /api/v1/admin/access-constraint-previews": {status: 200, body: `{"previewToken":"tok-1","commitBlocked":{"code":"constraint_admin_lockout","message":"mutation would lock out all constraint admins"}}`},
	}}
	_, err := (&Client{T: f}).CreateAccessConstraint(context.Background(), testDraft)
	if !errors.Is(err, ErrConstraintPreviewBlocked) || !strings.Contains(err.Error(), "lock out") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %v, want the preview only", f.calls)
	}
}

func TestDeleteAccessConstraintPreviewsTheRevision(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"POST /api/v1/admin/access-constraint-previews": {status: 200, body: `{"previewToken":"tok-2"}`},
		"DELETE /api/v1/admin/access-constraints/ac-1":  {status: 200, body: `{"auditId":"a2"}`},
	}}
	if err := (&Client{T: f}).DeleteAccessConstraint(context.Background(), "ac-1", "3"); err != nil {
		t.Fatal(err)
	}
	var prev scionPreviewRequest
	strictDecode(t, f.bodies["POST /api/v1/admin/access-constraint-previews"], &prev)
	if prev.Operation != "delete" || prev.ConstraintID != "ac-1" || prev.BaseRevision != "3" || prev.Draft != nil {
		t.Fatalf("preview = %+v", prev)
	}
	var del struct {
		PreviewToken string `json:"previewToken"`
	}
	strictDecode(t, f.bodies["DELETE /api/v1/admin/access-constraints/ac-1"], &del)
	if del.PreviewToken != "tok-2" {
		t.Fatalf("delete token = %q", del.PreviewToken)
	}
}

func TestAccessConstraintsNamedMatchesExactly(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/admin/access-constraints?nameContains=lever-remote-ceiling%3Aa%40b&pageSize=200": {status: 200, body: `{"items":[
			{"id":"ac-1","name":"lever-remote-ceiling:a@b","subject":{"kind":"principal","principalType":"user","principalId":"u1"},"scope":{"type":"system"},"status":"active","revision":"2"},
			{"id":"ac-2","name":"lever-remote-ceiling:xa@b","subject":{"kind":"principal","principalType":"user","principalId":"u2"},"scope":{"type":"system"},"status":"active","revision":"1"}
		],"totalCount":2}`},
		"GET /api/v1/admin/access-constraints?nameContains=x&pageSize=200": {status: 200, body: `{"constraints":[]}`},
	}}
	c := &Client{T: f}
	got, err := c.AccessConstraintsNamed(context.Background(), "lever-remote-ceiling:a@b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ac-1" || got[0].Revision != "2" || got[0].Subject.PrincipalID != "u1" {
		t.Fatalf("got %+v", got)
	}
	if _, err := c.AccessConstraintsNamed(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "items") {
		t.Fatalf("a list without items must be an error, got %v", err)
	}
}

const detailOld = `{"id":"ac-1","name":"lever-remote-ceiling:you@github","purpose":"kept",
	"subject":{"kind":"principal","principalType":"user","principalId":"u1"},"scope":{"type":"system"},
	"status":"active","revision":"4","appliesWhen":{"notBefore":null,"expiresAt":"2027-01-02T03:04:05Z"},
	"maximumPermissions":[{"id":"agent.read"},{"id":"user.list"}]}`

// The detail read keeps what an update must repeat: purpose, scope and the
// window, in the hub's own text.
func TestAccessConstraintReadsWhatAnUpdateRepeats(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"GET /api/v1/admin/access-constraints/ac-1": {status: 200, body: detailOld}}}
	got, err := (&Client{T: f}).AccessConstraint(context.Background(), "ac-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != "kept" || got.ScopeType != "system" || got.ScopeID != "" || got.Window == nil ||
		got.Window.NotBefore != nil || got.Window.ExpiresAt == nil || *got.Window.ExpiresAt != "2027-01-02T03:04:05Z" {
		t.Fatalf("got %+v", got)
	}
	d := got.Draft([]string{"agent.read"})
	if d.Name != got.Name || d.Purpose != "kept" || d.Subject != got.Subject || d.Scope.Type != "system" || d.AppliesWhen != got.Window {
		t.Fatalf("draft %+v", d)
	}
}

// An update previews with the constraint and the revision read, then PUTs the
// same draft with that revision in If-Match, as scion requires.
func TestUpdateAccessConstraintPreviewsThenCommitsAtTheRevision(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/admin/access-constraints/ac-1":     {status: 200, body: detailOld},
		"POST /api/v1/admin/access-constraint-previews": {status: 200, body: `{"previewToken":"tok-3"}`},
		"PUT /api/v1/admin/access-constraints/ac-1": {status: 200, body: `{"id":"ac-1","name":"lever-remote-ceiling:you@github",
			"subject":{"kind":"principal","principalType":"user","principalId":"u1"},"scope":{"type":"system"},
			"status":"active","revision":"5","maximumPermissions":[{"id":"agent.read"}],"auditId":"a3"}`},
	}}
	c := &Client{T: f}
	old, err := c.AccessConstraint(context.Background(), "ac-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.UpdateAccessConstraint(context.Background(), old.ID, old.Revision, old.Draft([]string{"agent.read"}))
	if err != nil {
		t.Fatal(err)
	}
	var prev scionPreviewRequest
	strictDecode(t, f.bodies["POST /api/v1/admin/access-constraint-previews"], &prev)
	var put scionUpdateRequest
	strictDecode(t, f.bodies["PUT /api/v1/admin/access-constraints/ac-1"], &put)
	if prev.Operation != "update" || prev.ConstraintID != "ac-1" || prev.BaseRevision != "4" || prev.Draft == nil || put.PreviewToken != "tok-3" {
		t.Fatalf("preview %+v, put token %q", prev, put.PreviewToken)
	}
	if f.ifMatch["PUT /api/v1/admin/access-constraints/ac-1"] != "4" {
		t.Fatalf("If-Match = %q, want the revision read", f.ifMatch["PUT /api/v1/admin/access-constraints/ac-1"])
	}
	pb, _ := json.Marshal(prev.Draft)
	cb, _ := json.Marshal(put.scionDraft)
	if string(pb) != string(cb) {
		t.Fatalf("commit draft %s differs from the previewed %s", cb, pb)
	}
	if put.Purpose != "kept" || put.AppliesWhen == nil || put.AppliesWhen.ExpiresAt == nil || put.AppliesWhen.NotBefore != nil ||
		!slices.Equal(put.MaximumPermissions, []string{"agent.read"}) {
		t.Fatalf("put %+v", put)
	}
	if got.Revision != "5" || !slices.Equal(got.MaximumPermissions, []string{"agent.read"}) {
		t.Fatalf("got %+v", got)
	}
}

// An update with no revision is refused before anything is sent.
func TestUpdateAccessConstraintNeedsARevision(t *testing.T) {
	f := &fakeDoer{}
	if _, err := (&Client{T: f}).UpdateAccessConstraint(context.Background(), "ac-1", "", testDraft); err == nil || len(f.calls) != 0 {
		t.Fatalf("err = %v, calls %v", err, f.calls)
	}
}

// A transport that cannot send If-Match fails the update before the PUT.
func TestUpdateAccessConstraintNeedsIfMatch(t *testing.T) {
	f := &bodyOnly{fakeDoer{replies: map[string]reply{
		"POST /api/v1/admin/access-constraint-previews": {status: 200, body: `{"previewToken":"tok-3"}`},
	}}}
	if _, err := (&Client{T: f}).UpdateAccessConstraint(context.Background(), "ac-1", "4", testDraft); err == nil || !strings.Contains(err.Error(), "If-Match") {
		t.Fatalf("err = %v", err)
	}
}

type bodyOnly struct{ f fakeDoer }

func (b *bodyOnly) Do(ctx context.Context, m, p string) (int, []byte, error) {
	return b.f.Do(ctx, m, p)
}
func (b *bodyOnly) DoBody(ctx context.Context, m, p string, body []byte) (int, []byte, error) {
	return b.f.DoBody(ctx, m, p, body)
}

// scion's refusal of a delete by an actor that lacks a permission of the
// constraint's maximum set (checkRelaxationAuthority) is recognised with the
// permissions it names; other refusals are not.
func TestRelaxationRefused(t *testing.T) {
	refusal := `{"error":{"code":"insufficient_constraint_relaxation_authority","message":"actor lacks authority over 1 permission(s) being restored","details":{"missingPermissionIds":["user.list"]}}}`
	f := &fakeDoer{replies: map[string]reply{
		"POST /api/v1/admin/access-constraint-previews": {status: 200, body: `{"previewToken":"tok-2"}`},
		"DELETE /api/v1/admin/access-constraints/ac-1":  {status: 403, body: refusal},
		"DELETE /api/v1/admin/access-constraints/ac-2":  {status: 403, body: `{"error":{"code":"forbidden","message":"no"}}`},
	}}
	c := &Client{T: f}
	err := c.DeleteAccessConstraint(context.Background(), "ac-1", "3")
	missing, ok := RelaxationRefused(err)
	if !ok || !slices.Equal(missing, []string{"user.list"}) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("missing %v, ok %v, err %v", missing, ok, err)
	}
	if _, ok := RelaxationRefused(c.DeleteAccessConstraint(context.Background(), "ac-2", "3")); ok {
		t.Fatal("a plain 403 is not a relaxation refusal")
	}
	if _, ok := RelaxationRefused(errors.New("insufficient_constraint_relaxation_authority")); ok {
		t.Fatal("only the hub's answer counts")
	}
}
