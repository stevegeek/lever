package agent

import (
	"context"
	"testing"
	"time"
)

func TestRenewReturnsFreshCertSameCN(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	id, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := Renew(context.Background(), env.Server.URL, id)
	if err != nil {
		t.Fatal(err)
	}
	if parseLeaf(t, renewed.CertPEM).Subject.CommonName != "worker" {
		t.Fatal("renewed cert must keep the authenticated CN")
	}
	if string(renewed.KeyPEM) == string(id.KeyPEM) {
		t.Fatal("renew must rotate the keypair")
	}
	if !ValidCert(renewed.CertPEM, time.Now()) {
		t.Fatal("renewed cert must be valid")
	}
}
