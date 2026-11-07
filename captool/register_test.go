package captool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/brokertest"
)

// fakeBroker also checks that every registration is first_party=true.
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

// An admin listener that accepts but never answers must not hold a call
// forever: the refresh is bounded and keeps the last good epoch. A caller
// hanging up does not cut it short (that would verify against a stale floor).
func TestFreshEpochGivesUpOnAStalledBroker(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)
	s := testServer(t)
	s.adminURL = stalled.URL
	s.epoch, s.epochTTL = 4, 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has hung up
	start := time.Now()
	if got := s.freshEpoch(ctx); got != 4 {
		t.Fatalf("freshEpoch = %d, want the last good 4", got)
	}
	took := time.Since(start)
	if took < epochRefreshTimeout-time.Second {
		t.Fatalf("freshEpoch gave up after %s: a hang-up cut the refresh short", took)
	}
	if took > epochRefreshTimeout+3*time.Second {
		t.Fatalf("freshEpoch took %s, past its bound", took)
	}
}
