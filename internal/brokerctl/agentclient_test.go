package brokerctl

// Simulated-agent helpers shared by the integration tests: a CA-issued
// client cert and an mTLS client that pins the broker CA.

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/stevegeek/lever/internal/broker/brokertest"
	"github.com/stevegeek/lever/internal/cap/ca"
)

// serverName is the orbstack HostToolAlias the tests issue the broker's
// server cert for.
const serverName = "host.orb.internal"

// workerClient dials 127.0.0.1 but verifies the server cert against
// serverName.
func workerClient(t *testing.T, caInst *ca.CA, cert tls.Certificate) *http.Client {
	t.Helper()
	return brokertest.Client(caInst, cert, serverName)
}

// workerCert mints a leaf directly from the CA, skipping provision/enrol.
func workerCert(t *testing.T, caInst *ca.CA, cn string) tls.Certificate {
	t.Helper()
	return brokertest.Cert(t, caInst, cn)
}
