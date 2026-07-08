package agent

import (
	"context"
	"testing"
)

func TestProvisionMintsWorkerTicket(t *testing.T) {
	env := testBroker(t)
	managerID := enrolManager(t, env.CA) // CN=manager; /provision is manager-CN-gated
	client, err := managerID.Client()
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := Provision(context.Background(), env.Server.URL, client, "worker")
	if err != nil {
		t.Fatalf("manager provision worker: %v", err)
	}
	if ticket == "" {
		t.Fatal("provision returned an empty ticket")
	}
	// The minted ticket must enrol a worker identity (CN==worker enforced by the broker).
	bs := BootstrapFor("worker", ticket, string(env.CA.CertPEM()), env.Server.URL)
	id, err := Enrol(context.Background(), bs.BrokerURL, []byte(bs.BrokerCA), bs.Ticket, bs.AgentCN)
	if err != nil {
		t.Fatalf("enrol worker with provisioned ticket: %v", err)
	}
	if parseLeaf(t, id.CertPEM).Subject.CommonName != "worker" {
		t.Fatal("worker cert CN must be 'worker'")
	}
}

func TestBootstrapForShape(t *testing.T) {
	bs := BootstrapFor("worker", "tok", "ca-pem", "https://b:8443")
	if bs.AgentCN != "worker" || bs.Ticket != "tok" || bs.BrokerCA != "ca-pem" || bs.BrokerURL != "https://b:8443" {
		t.Fatalf("BootstrapFor shape wrong: %+v", bs)
	}
}
