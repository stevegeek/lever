package broker

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// parseCSR decodes a PEM CSR and verifies its self-signature (proof of
// private-key possession). Shared by enrol and renew.
//
// SECURITY: this CheckSignature is the ONLY proof-of-possession check on
// both routes — each then calls SignPublicKey, which does not re-verify the
// CSR. Removing it would let a caller enrol or renew onto a public key whose
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
