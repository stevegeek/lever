package broker

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// parseCSR decodes a PEM CSR and verifies its self-signature.
//
// SECURITY: this CheckSignature is the ONLY proof-of-possession check on
// enrol and renew; SignPublicKey does not re-verify. Without it a caller
// could get a cert for a public key whose private half it does not hold.
//
// The error strings feed audit deny lines verbatim.
func parseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		return nil, fmt.Errorf("broker: invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("broker: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("broker: CSR signature: %w", err)
	}
	return csr, nil
}
