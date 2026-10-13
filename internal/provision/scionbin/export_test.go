package scionbin

import (
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// UseReleaseForTest makes Resolve use rel (nil: no release) and download
// from the TLS test servers, the first one serving the base URL, until the
// test ends.
func UseReleaseForTest(t *testing.T, rel *Release, servers ...*httptest.Server) {
	t.Helper()
	oldRel, oldSite := currentRelease, site
	t.Cleanup(func() { currentRelease, site = oldRel, oldSite })
	currentRelease = func() (*Release, error) { return rel, nil }
	if len(servers) == 0 {
		site = releaseSite{base: "https://release.invalid/dl/", hosts: map[string]bool{}}
		return
	}
	s := releaseSite{base: servers[0].URL + "/dl/", hosts: map[string]bool{}, transport: servers[0].Client().Transport}
	for _, srv := range servers {
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		s.hosts[u.Host] = true
	}
	site = s
}

// UseReleaseBaseForTest replaces the download base URL only.
func UseReleaseBaseForTest(t *testing.T, base string) {
	t.Helper()
	old := site.base
	t.Cleanup(func() { site.base = old })
	site.base = base
}

// LoadRelease reads the manifest under dir/ReleaseDir.
func LoadRelease(dir string) (*Release, error) { return releaseFrom(os.DirFS(dir)) }

// ProductionReleaseSite is the base URL and hosts the real download uses.
func ProductionReleaseSite() (string, map[string]bool) {
	return githubReleases.base, githubReleases.hosts
}

// SetStallTimeoutForTest shortens the download's stall timeout.
func SetStallTimeoutForTest(t *testing.T, d time.Duration) {
	old := stallTimeout
	t.Cleanup(func() { stallTimeout = old })
	stallTimeout = d
}

// EmbeddedRelease is the manifest go:embed carried into this test binary.
func EmbeddedRelease() (*Release, error) { return embeddedRelease() }
