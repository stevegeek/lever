package captool

import (
	"context"
	"testing"

	"github.com/stevegeek/lever/internal/broker/brokertest"
)

// fakeBroker is brokertest.FakeAdminServer plus the captool-specific check
// that every registration is first_party=true.
func fakeBroker(t *testing.T, epoch int64) *brokertest.FakeAdmin {
	t.Helper()
	f := brokertest.FakeAdminServer(t, epoch)
	t.Cleanup(func() {
		for _, body := range f.Registered() {
			if body["first_party"] != true {
				t.Errorf("captool must register first_party=true, got %v", body["first_party"])
			}
		}
	})
	return f
}

func TestRegisterCachesPubKeyAndEpoch(t *testing.T) {
	br := fakeBroker(t, 3)
	s := testServer(t)
	s.adminURL = br.Server.URL
	if err := s.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.pubKey == nil {
		t.Fatal("pubKey not cached")
	}
	if got := s.freshEpoch(context.Background()); got != 3 {
		t.Fatalf("freshEpoch = %d, want 3", got)
	}
}

func TestFreshEpochRefreshesAfterTTL(t *testing.T) {
	br := fakeBroker(t, 0)
	s := testServer(t)
	s.adminURL = br.Server.URL
	s.epochTTL = 0 // always stale -> always refetch
	_ = s.Register(context.Background())
	br.Epoch.Store(7) // broker bumps epoch
	if got := s.freshEpoch(context.Background()); got != 7 {
		t.Fatalf("freshEpoch = %d, want 7 after refresh", got)
	}
}
