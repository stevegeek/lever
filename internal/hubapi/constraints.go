package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Access constraints are scion's ceiling mechanism: a named maximum-permission
// set for a subject at a scope. A constraint only ever reduces what bindings
// grant; several on one principal intersect.
//
// These are hub-admin calls, made through AssertCurl as the admin identity.
// Every mutation is preview-bound: the hub issues a single-use token bound to
// the actor, the draft's hash and the hub's state, held only in that hub
// process's memory, so the preview and the commit must reach the same hub.

// ConstraintScopeSystem is scion's scope type for a constraint that applies
// at every scope, system and project alike.
const ConstraintScopeSystem = "system"

// ConstraintSubjectPrincipal is scion's subject kind for one exact principal.
const ConstraintSubjectPrincipal = "principal"

// ConstraintSubject mirrors scion's subjectSelectorRequest / resolvedSubject.
type ConstraintSubject struct {
	Kind          string `json:"kind"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
}

// ConstraintScope mirrors scion's constraintScopeRequest.
type ConstraintScope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// ConstraintDraft is the body of a create, and the draft of its preview.
type ConstraintDraft struct {
	Name               string            `json:"name"`
	Purpose            string            `json:"purpose"`
	Subject            ConstraintSubject `json:"subject"`
	Scope              ConstraintScope   `json:"scope"`
	MaximumPermissions []string          `json:"maximumPermissions"`
}

// AccessConstraint is what lever reads back about a constraint. The list
// leaves MaximumPermissions empty; the detail read fills it in.
type AccessConstraint struct {
	ID                 string
	Name               string
	Subject            ConstraintSubject
	ScopeType          string
	Status             string
	Revision           string
	MaximumPermissions []string
}

// wireConstraint is the hub's summary/detail row. resolvedScope names its id
// field projectId, not id, and each maximum permission is an object.
type wireConstraint struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Subject ConstraintSubject `json:"subject"`
	Scope   struct {
		Type string `json:"type"`
	} `json:"scope"`
	Status             string `json:"status"`
	Revision           string `json:"revision"`
	MaximumPermissions []struct {
		ID string `json:"id"`
	} `json:"maximumPermissions"`
}

func (w wireConstraint) constraint() AccessConstraint {
	c := AccessConstraint{ID: w.ID, Name: w.Name, Subject: w.Subject, ScopeType: w.Scope.Type,
		Status: w.Status, Revision: w.Revision}
	for _, p := range w.MaximumPermissions {
		c.MaximumPermissions = append(c.MaximumPermissions, p.ID)
	}
	return c
}

// ErrConstraintPreviewBlocked means the hub's preview said the change may not
// be committed (an incomplete impact analysis, or a constraint-admin lockout).
var ErrConstraintPreviewBlocked = errors.New("the hub's preview blocks this access constraint change")

// AccessConstraintsNamed lists the constraints whose name is exactly name
// (GET /api/v1/admin/access-constraints?nameContains=...). The hub filters by
// substring; the exact match is decided here.
func (c *Client) AccessConstraintsNamed(ctx context.Context, name string) ([]AccessConstraint, error) {
	q := url.Values{"nameContains": {name}, "pageSize": {"200"}}
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/admin/access-constraints?"+q.Encode(), &raw); err != nil {
		return nil, err
	}
	// Require the key: a renamed field would decode to "none" and the caller
	// would create a duplicate.
	field, ok := raw["items"]
	if !ok {
		return nil, &APIError{Msg: "access constraint list has no \"items\" field; the hub API changed shape"}
	}
	var items []wireConstraint
	if err := json.Unmarshal(field, &items); err != nil {
		return nil, &APIError{Msg: fmt.Sprintf("decoding access constraint list: %v", err)}
	}
	var out []AccessConstraint
	for _, w := range items {
		if w.Name == name {
			out = append(out, w.constraint())
		}
	}
	return out, nil
}

// AccessConstraint reads one constraint with its maximum permission set
// (GET /api/v1/admin/access-constraints/:id).
func (c *Client) AccessConstraint(ctx context.Context, id string) (AccessConstraint, error) {
	var w wireConstraint
	if err := c.get(ctx, "/api/v1/admin/access-constraints/"+url.PathEscape(id), &w); err != nil {
		return AccessConstraint{}, err
	}
	return w.constraint(), nil
}

func (c *Client) previewToken(ctx context.Context, req any) (string, error) {
	var res struct {
		PreviewToken  string `json:"previewToken"`
		CommitBlocked *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"commitBlocked"`
	}
	if err := c.send(ctx, http.MethodPost, "/api/v1/admin/access-constraint-previews", req, &res); err != nil {
		return "", err
	}
	if res.CommitBlocked != nil {
		return "", fmt.Errorf("%w: %s (%s)", ErrConstraintPreviewBlocked, res.CommitBlocked.Message, res.CommitBlocked.Code)
	}
	if res.PreviewToken == "" {
		return "", &APIError{Msg: "the access constraint preview carried no previewToken"}
	}
	return res.PreviewToken, nil
}

// CreateAccessConstraint previews and then commits a new constraint, and
// returns it as the hub stored it. The commit body repeats the previewed
// draft exactly: the token is bound to the draft's hash.
func (c *Client) CreateAccessConstraint(ctx context.Context, d ConstraintDraft) (AccessConstraint, error) {
	tok, err := c.previewToken(ctx, struct {
		Operation string          `json:"operation"`
		Draft     ConstraintDraft `json:"draft"`
	}{"create", d})
	if err != nil {
		return AccessConstraint{}, err
	}
	var w wireConstraint
	err = c.send(ctx, http.MethodPost, "/api/v1/admin/access-constraints", struct {
		ConstraintDraft
		PreviewToken string `json:"previewToken"`
	}{d, tok}, &w)
	if err != nil {
		return AccessConstraint{}, err
	}
	return w.constraint(), nil
}

// DeleteAccessConstraint previews and then commits the removal of a
// constraint at the revision lever read.
func (c *Client) DeleteAccessConstraint(ctx context.Context, id, revision string) error {
	tok, err := c.previewToken(ctx, struct {
		Operation    string `json:"operation"`
		ConstraintID string `json:"constraintId"`
		BaseRevision string `json:"baseRevision,omitempty"`
	}{"delete", id, revision})
	if err != nil {
		return err
	}
	return c.send(ctx, http.MethodDelete, "/api/v1/admin/access-constraints/"+url.PathEscape(id), struct {
		PreviewToken string `json:"previewToken"`
	}{tok}, nil)
}
