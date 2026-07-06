package captool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stevegeek/lever/internal/cap/token"
)

func fakeBroker(t *testing.T, epoch *int64) *httptest.Server {
	t.Helper()
	kp, _ := token.Generate()
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["first_party"] != true {
			t.Errorf("captool must register first_party=true, got %v", body["first_party"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"public_key": token.EncodePublicKey(kp.Public),
			"epoch":      int(atomic.LoadInt64(epoch)),
		})
	})
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"epoch": int(atomic.LoadInt64(epoch))})
	})
	return httptest.NewServer(mux)
}

func TestRegisterCachesPubKeyAndEpoch(t *testing.T) {
	var epoch int64 = 3
	br := fakeBroker(t, &epoch)
	defer br.Close()
	s := testServer(t)
	s.adminURL = br.URL
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
	var epoch int64 = 0
	br := fakeBroker(t, &epoch)
	defer br.Close()
	s := testServer(t)
	s.adminURL = br.URL
	s.epochTTL = 0 // always stale -> always refetch
	_ = s.Register(context.Background())
	atomic.StoreInt64(&epoch, 7) // broker bumps epoch
	if got := s.freshEpoch(context.Background()); got != 7 {
		t.Fatalf("freshEpoch = %d, want 7 after refresh", got)
	}
}
