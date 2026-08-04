package broker

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// parseCSR decodes a PEM CSR and verifies its self-signature (proof of
// private-key possession). Shared by enrol and renew.
//
// SECURITY: on /renew this CheckSignature is the ONLY proof-of-possession
// check — SignPublicKey does not re-verify the CSR (unlike enrol's SignCSR,
// which does). Removing it would let a caller renew onto a public key whose
// private half it does not hold.
//
// The error strings feed audit deny lines verbatim
// (TestRenewRejectsTamperedCSRSignature, TestRenewAndEnrolRejectGarbageCSRPEM).
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
