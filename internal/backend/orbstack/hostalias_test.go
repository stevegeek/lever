package orbstack

import (
	"context"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestResolveHostAliasParsesBothFamilies(t *testing.T) {
	f := exec.NewFakeRunner()
	// `orb -m <machine> getent ahosts host.orb.internal`
	f.Script("orb -m lever-jail getent ahosts host.orb.internal", exec.Result{Stdout: "" +
		"fd07:b51a:cc66:f0::fe STREAM host.orb.internal\n" +
		"fd07:b51a:cc66:f0::fe DGRAM  \n" +
		"0.250.250.254   STREAM \n" +
		"0.250.250.254   DGRAM  \n"})

	v4, v6, err := resolveHostAlias(context.Background(), f, "lever-jail")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if v4 != "0.250.250.254" {
		t.Fatalf("v4=%q", v4)
	}
	if v6 != "fd07:b51a:cc66:f0::fe" {
		t.Fatalf("v6=%q", v6)
	}
}
