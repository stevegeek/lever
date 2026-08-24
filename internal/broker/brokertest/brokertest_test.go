package brokertest

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stevegeek/lever/internal/cap/token"
)

func TestNewTestBrokerProvisionsAndRejectsStrangers(t *testing.T) {
	env := NewTestBroker(t, Config{})
	if ticket := env.ProvisionWorker(t, "worker"); ticket == "" {
		t.Fatal("empty ticket")
	}
	// A client with no cert must not reach /provision.
	resp, err := env.ClientFor(t, "nobody").Post(env.Server.URL+"/provision", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a non-manager identity must not provision")
	}
}

func TestNewTestBrokerHonoursConfig(t *testing.T) {
	env := NewTestBroker(t, Config{Workers: []string{"alpha"}, ManagerIdentity: "boss"})
	c := Client(env.CA, Cert(t, env.CA, "boss"), "")
	if ticket := ProvisionWorker(t, c, env.Server.URL, "alpha"); ticket == "" {
		t.Fatal("empty ticket")
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
