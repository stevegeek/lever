package brokerctl

import (
	"testing"

	"github.com/lever-to/lever/internal/broker"
)

func TestEnsureKeysGeneratesThenReloads(t *testing.T) {
	s := StateDir(t.TempDir())
	kp1, ca1, err := s.EnsureKeys()
	if err != nil {
		t.Fatal(err)
	}
	kp2, ca2, err := s.EnsureKeys() // second call must reload, not regenerate
	if err != nil {
		t.Fatal(err)
	}
	if string(kp1.Public) != string(kp2.Public) {
		t.Fatal("capability-signing key changed across EnsureKeys calls — must persist + reload")
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Fatal("CA cert changed across EnsureKeys calls — must persist + reload")
	}
}

func TestRevocationRoundTrip(t *testing.T) {
	s := StateDir(t.TempDir())
	if _, _, err := s.EnsureKeys(); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.LoadRevocation(); rs.MinEpoch != 0 || len(rs.Revoked) != 0 {
		t.Fatalf("absent revocation.json must be zero value, got %+v", rs)
	}
	if err := s.SaveRevocation(broker.RevocationState{MinEpoch: 3, Revoked: []string{"worker"}}); err != nil {
		t.Fatal(err)
	}
	rs, err := s.LoadRevocation()
	if err != nil {
		t.Fatal(err)
	}
	if rs.MinEpoch != 3 || len(rs.Revoked) != 1 || rs.Revoked[0] != "worker" {
		t.Fatalf("revocation did not round-trip: %+v", rs)
	}
}
