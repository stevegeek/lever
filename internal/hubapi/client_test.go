package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// reply is one canned response keyed by "METHOD /path".
type reply struct {
	status int
	body   string
	err    error
}

// fakeDoer answers from a script and records what was asked.
type fakeDoer struct {
	replies map[string]reply
	// seq lets a key answer differently on successive calls (the DELETE then
	// verify-read sequence).
	seq   map[string][]reply
	calls []string
}

func (f *fakeDoer) Do(_ context.Context, method, path string) (int, []byte, error) {
	key := method + " " + path
	f.calls = append(f.calls, key)
	if rs, ok := f.seq[key]; ok && len(rs) > 0 {
		r := rs[0]
		f.seq[key] = rs[1:]
		return r.status, []byte(r.body), r.err
	}
	r, ok := f.replies[key]
	if !ok {
		return 0, nil, fmt.Errorf("fakeDoer: unscripted call %s", key)
	}
	return r.status, []byte(r.body), r.err
}

const (
	leverUUID    = "22222222-2222-2222-2222-222222222222"
	projectsBody = `{"projects":[
	  {"id":"11111111-1111-1111-1111-111111111111","name":"other","slug":"other"},
	  {"id":"22222222-2222-2222-2222-222222222222","name":"lever","slug":"lever-1"}
	],"totalCount":2}`
)

// stripScript is the happy path: list projects, DELETE succeeds, verify read
// comes back clean.
func stripScript() *fakeDoer {
	return &fakeDoer{
		replies: map[string]reply{
			"GET /api/v1/projects": {status: 200, body: projectsBody},
			"DELETE /api/v1/projects/" + leverUUID + "/shared-dirs/scratchpad": {status: 204},
			"GET /api/v1/projects/" + leverUUID + "/shared-dirs":               {status: 200, body: `{"sharedDirs":[]}`},
		},
	}
}

func TestProjectIDMatchesNameOrSlug(t *testing.T) {
	for _, key := range []string{"lever", "lever-1"} {
		c := &Client{T: stripScript()}
		id, err := c.ProjectID(context.Background(), key, "hub")
		if err != nil {
			t.Fatalf("ProjectID(%q): %v", key, err)
		}
		if id != leverUUID {
			t.Errorf("ProjectID(%q) = %q, want the lever project UUID", key, id)
		}
	}
}

func TestProjectIDUnknownNameNamesTheHub(t *testing.T) {
	// When the jail reaches a hub but the wrong one, this error is the
	// operator's only clue, so it must name the endpoint.
	c := &Client{T: stripScript()}
	_, err := c.ProjectID(context.Background(), "absent", "http://127.0.0.1:8080")
	if err == nil {
		t.Fatal("expected an error for a project the hub does not list")
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:8080") {
		t.Errorf("error should name the hub endpoint, got %v", err)
	}
}

func TestStripSharedDirDeletesThenVerifies(t *testing.T) {
	f := stripScript()
	if err := (&Client{T: f}).StripSharedDir(context.Background(), "lever", "hub", "scratchpad"); err != nil {
		t.Fatalf("StripSharedDir: %v", err)
	}
	want := []string{
		"GET /api/v1/projects",
		"DELETE /api/v1/projects/" + leverUUID + "/shared-dirs/scratchpad",
		"GET /api/v1/projects/" + leverUUID + "/shared-dirs",
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, f.calls[i], want[i])
		}
	}
}

func TestStripSharedDirIsIdempotent(t *testing.T) {
	// A second apply: the hub answers 404 because the dir is already gone. The
	// verify read confirms it, so this is a success.
	f := stripScript()
	f.replies["DELETE /api/v1/projects/"+leverUUID+"/shared-dirs/scratchpad"] =
		reply{status: 404, body: `{"error":"Shared directory not found"}`}
	if err := (&Client{T: f}).StripSharedDir(context.Background(), "lever", "hub", "scratchpad"); err != nil {
		t.Fatalf("a 404 with a clean verify read must succeed, got %v", err)
	}
}

func TestStripSharedDirFailsWhen404ButDirStillThere(t *testing.T) {
	// The bug the verify read exists to catch: the hub answers 404 for "no such
	// route" and for "no such project" too, so a moved endpoint would otherwise
	// read as a successful strip while every agent keeps the writable mount.
	f := stripScript()
	f.replies["DELETE /api/v1/projects/"+leverUUID+"/shared-dirs/scratchpad"] = reply{status: 404}
	f.replies["GET /api/v1/projects/"+leverUUID+"/shared-dirs"] =
		reply{status: 200, body: `{"sharedDirs":[{"name":"scratchpad"}]}`}

	err := (&Client{T: f}).StripSharedDir(context.Background(), "lever", "hub", "scratchpad")
	if err == nil {
		t.Fatal("a 404 that did not actually remove the dir must fail")
	}
	if !strings.Contains(err.Error(), "still on project") {
		t.Errorf("error should say the dir survived, got %v", err)
	}
}

func TestStripSharedDirSurfacesForbidden(t *testing.T) {
	// 403 means the PAT lacks project:update. It must fail loud — a silent pass
	// would leave the cross-agent mount in place.
	f := stripScript()
	f.replies["DELETE /api/v1/projects/"+leverUUID+"/shared-dirs/scratchpad"] =
		reply{status: http.StatusForbidden, body: `{"error":"Forbidden"}`}
	err := (&Client{T: f}).StripSharedDir(context.Background(), "lever", "hub", "scratchpad")
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the status, got %v", err)
	}
}

func TestStripSharedDirSurfacesTransportFailure(t *testing.T) {
	// The jail is down. Status 0 must never be read as a status.
	f := stripScript()
	f.replies["GET /api/v1/projects"] = reply{err: errors.New("connection refused")}
	err := (&Client{T: f}).StripSharedDir(context.Background(), "lever", "hub", "scratchpad")
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("transport failure must surface, got %v", err)
	}
}

func TestStripSharedDirFailsWhenVerifyReadFails(t *testing.T) {
	// A DELETE that looked fine but an unreadable verify must NOT pass: lever
	// cannot claim the mount is gone if it could not look.
	f := stripScript()
	f.replies["GET /api/v1/projects/"+leverUUID+"/shared-dirs"] = reply{status: 500, body: "boom"}
	err := (&Client{T: f}).StripSharedDir(context.Background(), "lever", "hub", "scratchpad")
	if err == nil || !strings.Contains(err.Error(), "verifying") {
		t.Fatalf("an unreadable verify must fail, got %v", err)
	}
}

func TestSharedDirsListsProjectMounts(t *testing.T) {
	f := stripScript()
	f.replies["GET /api/v1/projects/"+leverUUID+"/shared-dirs"] =
		reply{status: 200, body: `{"sharedDirs":[{"name":"scratchpad","readOnly":false,"inWorkspace":false}]}`}
	dirs, err := (&Client{T: f}).SharedDirs(context.Background(), leverUUID)
	if err != nil {
		t.Fatalf("SharedDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0].Name != "scratchpad" || dirs[0].ReadOnly {
		t.Fatalf("dirs = %+v, want one writable scratchpad", dirs)
	}
}

func TestGetSurfacesNonJSONBody(t *testing.T) {
	// A wrong service on the hub port returns HTML, not scion's envelope.
	f := stripScript()
	f.replies["GET /api/v1/projects"] = reply{status: 200, body: "<html>not scion</html>"}
	_, err := (&Client{T: f}).ProjectID(context.Background(), "lever", "hub")
	if err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("a non-JSON body must fail loud, got %v", err)
	}
}
