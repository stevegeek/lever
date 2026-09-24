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

// The bodies are decoded the way scion decodes them: readJSONStrict rejects
// unknown fields (pkg/hub/handlers_access_constraints.go). These structs copy
// scion's request types field for field.
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
