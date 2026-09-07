package brokertest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stevegeek/lever/internal/cap/token"
)

func TestNewTestBrokerMintsWorkerTicketsHostSide(t *testing.T) {
	env := NewTestBroker(t, Config{})
	if ticket := env.WorkerTicket(t, "worker"); ticket == "" {
		t.Fatal("empty ticket")
	}
	// No agent-facing route mints a worker ticket: /provision is gone from the
	// jail listener, whoever asks.
	body := []byte(`{"worker":"worker"}`)
	resp, err := env.ClientFor(t, "manager").Post(env.Server.URL+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/provision on the jail listener: status %d, want 404", resp.StatusCode)
	}
}

func TestNewTestBrokerHonoursConfig(t *testing.T) {
	env := NewTestBroker(t, Config{Workers: []string{"alpha"}, ManagerIdentity: "boss"})
	if ticket := env.WorkerTicket(t, "alpha"); ticket == "" {
		t.Fatal("empty ticket")
	}
	if env.Tickets.Last("worker") != nil {
		t.Fatal("an undeclared worker must have nothing staged")
	}
}

func TestFakeAdminServerRecordsRegistrationsAndEpoch(t *testing.T) {
	f := FakeAdminServer(t, 3)
	resp, err := http.Post(f.Server.URL+"/register", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		PublicKey string `json:"public_key"`
		Epoch     int    `json:"epoch"`
	}
	if err := decode(resp, &got); err != nil {
		t.Fatal(err)
	}
	if got.PublicKey != token.EncodePublicKey(f.Keys.Public) || got.Epoch != 3 {
		t.Fatalf("register = %+v", got)
	}
	if n := len(f.Registered()); n != 1 {
		t.Fatalf("registered = %d, want 1", n)
	}
	f.Epoch.Store(7)
	resp, err = http.Get(f.Server.URL + "/epoch")
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(resp, &got); err != nil {
		t.Fatal(err)
	}
	if got.Epoch != 7 {
		t.Fatalf("epoch = %d, want 7", got.Epoch)
	}
}

func decode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}
