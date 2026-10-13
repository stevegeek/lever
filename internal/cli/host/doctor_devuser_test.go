package host

import (
	"context"
	"errors"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
)

func devUserApp(logins ...string) *config.App {
	return &config.App{Remote: config.Remote{Enabled: true, AllowedUsers: remoteUsers(logins...)}}
}

func meFn(u hubapi.User, err error) func(context.Context) (hubapi.User, error) {
	return func(context.Context) (hubapi.User, error) { return u, err }
}

func TestCheckDevUserIdentity(t *testing.T) {
	ctx := context.Background()

	// Remote off: no row at all.
	if r := checkDevUserIdentity(ctx, &config.App{}, meFn(hubapi.User{}, nil)); r.name != "" {
		t.Fatalf("remote off should add no row, got %+v", r)
	}

	// A hub that cannot be reached is not a finding here.
	if r := checkDevUserIdentity(ctx, devUserApp("op"), meFn(hubapi.User{}, errors.New("down"))); r.name != "" {
		t.Fatalf("unreachable hub should add no row, got %+v", r)
	}

	// The dev row carries its own address: pass.
	ok := checkDevUserIdentity(ctx, devUserApp("op"),
		meFn(hubapi.User{ID: config.ScionDevUserID, Email: "dev@localhost"}, nil))
	if !ok.ok {
		t.Fatalf("want a pass for an untouched dev row, got %+v", ok)
	}

	// The dev row now carries a configured remote login's email: the finding.
	victim := config.HubEmailFor("op")
	bad := checkDevUserIdentity(ctx, devUserApp("op"),
		meFn(hubapi.User{ID: config.ScionDevUserID, Email: victim}, nil))
	if bad.ok || bad.fix == "" {
		t.Fatalf("want a failing row for a renamed dev user, got %+v", bad)
	}

	// Case-insensitive, since scion matches emails EqualFold.
	bad2 := checkDevUserIdentity(ctx, devUserApp("Op@Example.Test"),
		meFn(hubapi.User{ID: config.ScionDevUserID, Email: "op@example.test"}, nil))
	if bad2.ok {
		t.Fatalf("want a finding for a case-different match, got %+v", bad2)
	}
}
