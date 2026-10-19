package imageref

import (
	"strings"
	"testing"
)

func TestAgent(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"scionlocal/lever-claude:amd64", "lever.local/scionlocal/lever-claude:amd64"},
		{"lever.local/lever-claude:amd64", "lever.local/lever-claude:amd64"},
		{"myimage:1", "lever.local/myimage:1"},
		{"myorg/tool:2", "lever.local/myorg/tool:2"},
		{"localhost/x:1", "lever.local/localhost/x:1"},
		{"scionlocalevil/x:1", "lever.local/scionlocalevil/x:1"},
		{"ghcr.io/org/x:1", "ghcr.io/org/x:1"},
		{"registry:5000/x:1", "registry:5000/x:1"},
		{"scionlocal/x@sha256:abc", "scionlocal/x@sha256:abc"},
	} {
		if got := Agent(c.in); got != c.want {
			t.Errorf("Agent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// scionRewrite mirrors scion's RewriteImageRegistry (pkg/config/settings_v1.go
// at the pinned commit), which the hub's dispatch and the broker apply to every
// agent image.
func scionRewrite(ref, registry string) string {
	if i := strings.Index(ref, "/"); i >= 0 && strings.ContainsAny(ref[:i], ".:") {
		return ref
	}
	return registry + "/" + ref[strings.LastIndex(ref, "/")+1:]
}

// TestAgentNeverRewritten: whatever image_registry an agent sets on the hub,
// the name an agent image runs under keeps lever's registry.
func TestAgentNeverRewritten(t *testing.T) {
	for _, in := range []string{"scionlocal/lever-claude-slim:arm64", "lever-claude:amd64", "a/b/c:1", "localhost/x:1"} {
		if scionRewrite(in, "ghcr.io/attacker") == in {
			t.Fatalf("the mirror of scion's rule does not rewrite %q", in)
		}
		out := Agent(in)
		if got := scionRewrite(out, "ghcr.io/attacker"); got != out {
			t.Errorf("%q runs as %q, which the hub rewrites to %q", in, out, got)
		}
		if !Qualified(out) {
			t.Errorf("%q runs as %q, which is not qualified", in, out)
		}
	}
}

func TestNormalize(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"x", "docker.io/library/x:latest"},
		{"x:arm64", "docker.io/library/x:arm64"},
		{"docker.io/library/x:arm64", "docker.io/library/x:arm64"},
		{"docker.io/x:arm64", "docker.io/library/x:arm64"},
		{"index.docker.io/library/x:arm64", "docker.io/library/x:arm64"},
		{"Docker.IO/x:arm64", "docker.io/library/x:arm64"},
		{"myorg/x", "docker.io/myorg/x:latest"},
		{"Lever.Local/x:1", "lever.local/x:1"},
		{"localhost/x", "localhost/x:latest"},
		{"registry:5000/x", "registry:5000/x:latest"},
		{"registry:5000/x:2", "registry:5000/x:2"},
		{"x@sha256:abc", "docker.io/library/x@sha256:abc"},
		{"ghcr.io/o/x:1@sha256:abc", "ghcr.io/o/x:1@sha256:abc"},
	} {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Two spellings of one image share a name; distinct images do not.
func TestNames(t *testing.T) {
	share := func(a, b string) bool {
		for _, x := range Names(a) {
			for _, y := range Names(b) {
				if x == y {
					return true
				}
			}
		}
		return false
	}
	for _, c := range [][2]string{
		{"x:arm64", "docker.io/library/x:arm64"},
		{"x:arm64", "index.docker.io/library/x:arm64"},
		{"x:arm64", "lever.local/x:arm64"},
		{"x:arm64", "LEVER.LOCAL/x:arm64"},
		{"x:arm64", "localhost/x:arm64"},
		{"x", "docker.io/library/x:latest"},
		{"myorg/x:1", "docker.io/myorg/x:1"},
	} {
		if !share(c[0], c[1]) {
			t.Errorf("%q and %q share no name: %q, %q", c[0], c[1], Names(c[0]), Names(c[1]))
		}
	}
	for _, c := range [][2]string{
		{"x:arm64", "y:arm64"},
		{"x:arm64", "x:amd64"},
		{"lever.local/x:1", "ghcr.io/o/x:1"},
		{"myorg/x:1", "x:1"},
	} {
		if share(c[0], c[1]) {
			t.Errorf("%q and %q share a name: %q, %q", c[0], c[1], Names(c[0]), Names(c[1]))
		}
	}
}
