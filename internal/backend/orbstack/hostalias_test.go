package orbstack

import (
	"context"
	"testing"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
)

func TestResolveHostAliasParsesBothFamilies(t *testing.T) {
	f := proc.NewFakeRunner()
	// `orb -m <machine> getent ahosts host.orb.internal`
	f.Script(orbGuest.User+" getent ahosts "+orbGuest.Alias, proc.Result{Stdout: "" +
		"fd07:b51a:cc66:f0::fe STREAM host.orb.internal\n" +
		"fd07:b51a:cc66:f0::fe DGRAM  \n" +
		"0.250.250.254   STREAM \n" +
		"0.250.250.254   DGRAM  \n"})

	v4, v6, err := resolveHostAlias(context.Background(), f, machine)
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

type limitRecorder struct {
	*proc.FakeRunner
	limit int
}

func (r *limitRecorder) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	r.limit, _ = proc.OutputLimit(ctx)
	return r.FakeRunner.Run(ctx, env, name, args...)
}

func TestResolveHostAliasRunsUnderTheGuestOutputLimit(t *testing.T) {
	r := &limitRecorder{FakeRunner: proc.NewFakeRunner()}
	r.Script(orbGuest.User+" getent ahosts "+orbGuest.Alias, proc.Result{Stdout: "0.250.250.254   STREAM \n"})
	if _, _, err := resolveHostAlias(context.Background(), r, machine); err != nil || r.limit != guest.OutputLimit {
		t.Fatalf("limit %d, err %v", r.limit, err)
	}
}
