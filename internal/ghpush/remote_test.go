package ghpush

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testRemote serves bare repos under root over git's smart HTTP protocol
// (git http-backend via net/http/cgi) and records Authorization headers.
type testRemote struct {
	root  string
	srv   *httptest.Server
	mu    sync.Mutex
	auths []string
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "credential.helper="}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newTestRemote creates owner/name.git with one commit on main and returns
// the remote plus a work clone the test builds bundles from.
func newTestRemote(t *testing.T, repo string) (*testRemote, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	exe := strings.TrimSpace(gitT(t, ".", "--exec-path"))
	backend := filepath.Join(exe, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available")
	}
	root := t.TempDir()
	bare := filepath.Join(root, repo+".git")
	gitT(t, root, "init", "--bare", "-b", "main", bare)
	gitT(t, bare, "config", "http.receivepack", "true")
	gitT(t, bare, "config", "receive.fsckObjects", "true")
	gitT(t, bare, "config", "receive.denyNonFastForwards", "true")
	work := t.TempDir()
	gitT(t, work, "init", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644)
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-m", "init")
	gitT(t, work, "push", bare, "main")
	gitT(t, work, "remote", "add", "origin", bare)
	gitT(t, work, "fetch", "origin")
	r := &testRemote{root: root}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r, work
}

func (r *testRemote) headSHA(t *testing.T, repo, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", filepath.Join(r.root, repo+".git"), "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
