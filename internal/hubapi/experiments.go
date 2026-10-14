package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Experiment is one row of scion's admin experiments view. Override is nil
// when the hub stores none for the name, so the compiled default applies.
type Experiment struct {
	Name     string `json:"name"`
	Override *bool  `json:"override"`
	Enabled  bool   `json:"enabled"`
}

// Experiments is scion's admin experiments view (GET /api/v1/admin/experiments).
// Malformed means the stored overrides are unreadable: scion then resolves
// every server-layer experiment off and refuses writes until an admin resets
// the section.
type Experiments struct {
	Revision  int64
	Malformed bool
	Items     []Experiment
}

// Lookup returns the row for name, or false when this hub does not register
// it.
func (e Experiments) Lookup(name string) (Experiment, bool) {
	for _, it := range e.Items {
		if it.Name == name {
			return it, true
		}
	}
	return Experiment{}, false
}

// ErrNoExperiments means the hub has no experiments admin route (HTTP 404): a
// scion that predates hub-wide experiments.
var ErrNoExperiments = errors.New("the hub has no experiments admin route")

// ErrExperimentsConflict is SetExperimentOverrides' answer when the stored
// revision is not the expected one (HTTP 409 revision_conflict).
var ErrExperimentsConflict = errors.New("the hub's experiment settings changed concurrently")

const adminExperimentsPath = "/api/v1/admin/experiments"

// AdminExperiments reads the hub's experiments view. It is a hub-admin call
// (hub.experiments.update); no lever token can make it.
func (c *Client) AdminExperiments(ctx context.Context) (Experiments, error) {
	status, body, err := c.do(ctx, http.MethodGet, adminExperimentsPath)
	if err != nil {
		return Experiments{}, err
	}
	if status == http.StatusNotFound {
		return Experiments{}, ErrNoExperiments
	}
	if status < 200 || status > 299 {
		return Experiments{}, &APIError{Status: status,
			Msg: fmt.Sprintf("GET %s: HTTP %d: %s", adminExperimentsPath, status, snippet(body))}
	}
	return decodeExperiments(http.MethodGet, body)
}

// SetExperimentOverrides writes overrides as one compare-and-set against
// expectedRevision. scion merges them into the stored set: names not sent
// keep their override.
func (c *Client) SetExperimentOverrides(ctx context.Context, overrides map[string]bool, expectedRevision int64) (Experiments, error) {
	if len(overrides) == 0 {
		return Experiments{}, errors.New("hubapi: no experiment overrides to write")
	}
	in, err := json.Marshal(struct {
		Overrides        map[string]bool `json:"overrides"`
		ExpectedRevision int64           `json:"expected_revision"`
	}{overrides, expectedRevision})
	if err != nil {
		return Experiments{}, fmt.Errorf("hubapi: encoding experiment overrides: %w", err)
	}
	status, body, err := c.doBody(ctx, http.MethodPut, adminExperimentsPath, in)
	if err != nil {
		return Experiments{}, err
	}
	if status == http.StatusNotFound {
		return Experiments{}, ErrNoExperiments
	}
	if status == http.StatusConflict && errorCode(body) == "revision_conflict" {
		return Experiments{}, ErrExperimentsConflict
	}
	if status < 200 || status > 299 {
		return Experiments{}, &APIError{Status: status,
			Msg: fmt.Sprintf("PUT %s: HTTP %d: %s", adminExperimentsPath, status, snippet(body))}
	}
	return decodeExperiments(http.MethodPut, body)
}

// decodeExperiments requires every field lever reads: a renamed one would
// otherwise decode as "no override" or "revision 0" and lever would act on it.
func decodeExperiments(method string, body []byte) (Experiments, error) {
	var raw struct {
		Revision    *int64       `json:"revision"`
		Malformed   *bool        `json:"malformed"`
		Experiments []Experiment `json:"experiments"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Experiments{}, &APIError{Msg: fmt.Sprintf("%s %s: decoding response: %v", method, adminExperimentsPath, err)}
	}
	if raw.Revision == nil || raw.Malformed == nil || raw.Experiments == nil {
		return Experiments{}, &APIError{Msg: fmt.Sprintf("%s %s: the response lacks revision, malformed or experiments; the hub API changed shape", method, adminExperimentsPath)}
	}
	return Experiments{Revision: *raw.Revision, Malformed: *raw.Malformed, Items: raw.Experiments}, nil
}

func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Error.Code
}

// RouteServed reports whether the hub serves path to this caller, from a GET.
// It is for routes scion answers 404 exactly while a feature is off, and older
// scions answer 404 for a route they lack, so 404 is "not served" and any other
// answer is "served". 401 says nothing about the route and is an APIError.
func (c *Client) RouteServed(ctx context.Context, path string) (bool, error) {
	status, body, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized:
		return false, &APIError{Status: status,
			Msg: fmt.Sprintf("GET %s: HTTP %d: %s", path, status, snippet(body))}
	}
	return true, nil
}
