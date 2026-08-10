package hubapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hubStub records the requests it serves so tests can assert the exact URLs and
// auth header lever sends.
type hubStub struct {
	*httptest.Server
	methods []string
	paths   []string
	auth    []string
}

func newHubStub(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *hubStub {
	t.Helper()
	s := &hubStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.methods = append(s.methods, r.Method)
		s.paths = append(s.paths, r.URL.Path)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *hubStub) client(token string) *Client {
	return &Client{BaseURL: s.URL, Token: func() string { return token }}
}

const projectsBody = `{"projects":[
  {"id":"11111111-1111-1111-1111-111111111111","name":"other","slug":"other"},
  {"id":"22222222-2222-2222-2222-222222222222","name":"lever","slug":"lever-1"}
],"totalCount":2}`

func TestProjectIDMatchesNameOrSlug(t *testing.T) {
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(projectsBody))
	})
	c := stub.client("pat-abc")

	for _, key := range []string{"lever", "lever-1"} {
		id, err := c.ProjectID(context.Background(), key)
		if err != nil {
			t.Fatalf("ProjectID(%q): %v", key, err)
		}
		if id != "22222222-2222-2222-2222-222222222222" {
			t.Errorf("ProjectID(%q) = %q, want the lever project UUID", key, id)
		}
	}
	if stub.auth[0] != "Bearer pat-abc" {
		t.Errorf("Authorization = %q, want the controller PAT as a bearer token", stub.auth[0])
	}
}

func TestProjectIDUnknownName(t *testing.T) {
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(projectsBody))
	})
	if _, err := stub.client("t").ProjectID(context.Background(), "absent"); err == nil {
		t.Fatal("expected an error for a project the hub does not list")
	}
}

func TestStripSharedDirDeletesByResolvedUUID(t *testing.T) {
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(projectsBody))
	})

	if err := stub.client("t").StripSharedDir(context.Background(), "lever", "scratchpad"); err != nil {
		t.Fatalf("StripSharedDir: %v", err)
	}

	want := "/api/v1/projects/22222222-2222-2222-2222-222222222222/shared-dirs/scratchpad"
	if len(stub.paths) != 2 || stub.paths[1] != want {
		t.Fatalf("paths = %v, want a list then DELETE %s", stub.paths, want)
	}
	if stub.methods[1] != http.MethodDelete {
		t.Errorf("second call = %s, want DELETE", stub.methods[1])
	}
}

func TestStripSharedDirIsIdempotent(t *testing.T) {
	// The hub answers 404 when the project carries no such shared dir. A second
	// apply must not fail over work the first one already did.
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, `{"error":"Shared directory not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(projectsBody))
	})
	if err := stub.client("t").StripSharedDir(context.Background(), "lever", "scratchpad"); err != nil {
		t.Fatalf("a 404 DELETE must count as success, got %v", err)
	}
}

func TestStripSharedDirSurfacesForbidden(t *testing.T) {
	// 403 means the PAT lacks project:update. That must fail loud — a silent
	// pass would leave the cross-agent mount in place.
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, `{"error":"Forbidden"}`, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(projectsBody))
	})
	err := stub.client("t").StripSharedDir(context.Background(), "lever", "scratchpad")
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the status, got %v", err)
	}
}

func TestSharedDirsListsProjectMounts(t *testing.T) {
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sharedDirs":[{"name":"scratchpad","readOnly":false,"inWorkspace":false}]}`))
	})
	dirs, err := stub.client("t").SharedDirs(context.Background(), "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("SharedDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0].Name != "scratchpad" || dirs[0].ReadOnly {
		t.Fatalf("dirs = %+v, want one writable scratchpad", dirs)
	}
	want := "/api/v1/projects/22222222-2222-2222-2222-222222222222/shared-dirs"
	if stub.paths[0] != want {
		t.Errorf("path = %q, want %q", stub.paths[0], want)
	}
}

func TestNoTokenOmitsAuthHeader(t *testing.T) {
	// An unminted PAT must not send an empty bearer header — the hub reads that
	// as a malformed credential rather than as no credential.
	stub := newHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(projectsBody))
	})
	c := &Client{BaseURL: stub.URL, Token: func() string { return "" }}
	if _, err := c.ProjectID(context.Background(), "lever"); err != nil {
		t.Fatalf("ProjectID: %v", err)
	}
	if stub.auth[0] != "" {
		t.Errorf("Authorization = %q, want no header", stub.auth[0])
	}
}
