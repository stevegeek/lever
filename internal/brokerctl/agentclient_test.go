package brokerctl

// agentclient_test.go holds the simulated-agent helpers shared by the
// integration tests, on top of brokertest: a CA-issued client cert and an
// mTLS client that pins the broker CA.

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/stevegeek/lever/internal/broker/brokertest"
	"github.com/stevegeek/lever/internal/cap/ca"
)

// serverName is the orbstack HostToolAlias, which is what these integration
// tests issue the broker's server cert for and verify against.
const serverName = "host.orb.internal"

// workerClient builds an mTLS client that pins the broker CA and presents the
// worker's CA-issued cert, dialing 127.0.0.1 but verifying the server cert
// against the broker's serverName (host.orb.internal) — the OrbStack hostname the
// real server cert is issued for.
func workerClient(t *testing.T, caInst *ca.CA, cert tls.Certificate) *http.Client {
	t.Helper()
	return brokertest.Client(caInst, cert, serverName)
}

// workerCert issues a CA-signed client cert for cn (the simulated-agent technique
// from the broker e2e: skip provision/enrol, mint the leaf directly from the CA).
func workerCert(t *testing.T, caInst *ca.CA, cn string) tls.Certificate {
	t.Helper()
	return brokertest.Cert(t, caInst, cn)
}
