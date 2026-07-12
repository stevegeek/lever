package ca

import (
	"sync"
	"testing"
	"time"
)

func TestServerCertSourceStableWithinMargin(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	src, err := c.NewServerCertSource("host.orb.internal", []string{"host.orb.internal"}, nil)
	if err != nil {
		t.Fatalf("NewServerCertSource: %v", err)
	}
	a, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	b, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if a.Leaf == nil || b.Leaf == nil {
		t.Fatal("source must return certs with a parsed Leaf")
	}
	if a.Leaf.SerialNumber.Cmp(b.Leaf.SerialNumber) != 0 {
		t.Fatal("fresh cert must be reused across handshakes, not re-minted every call")
	}
}

func TestServerCertSourceRotatesNearExpiry(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	src, err := c.NewServerCertSource("host.orb.internal", []string{"host.orb.internal"}, []string{"198.51.100.7"})
	if err != nil {
		t.Fatalf("NewServerCertSource: %v", err)
	}
	first, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	// Warp to inside the rotation margin: less than rotateMargin left on the leaf.
	src.now = func() time.Time { return first.Leaf.NotAfter.Add(-rotateMargin / 2) }
	second, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate near expiry: %v", err)
	}
	if second.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) == 0 {
		t.Fatal("cert inside the rotation margin must be re-minted")
	}
	// The rotated cert keeps the SANs and chains to the CA. (NotAfter is not
	// compared: mint uses the real clock, so old and new expiries coincide when
	// the test warps only the source's view of now.)
	if err := second.Leaf.VerifyHostname("host.orb.internal"); err != nil {
		t.Fatalf("rotated cert DNS SAN: %v", err)
	}
	if err := second.Leaf.VerifyHostname("198.51.100.7"); err != nil {
		t.Fatalf("rotated cert IP SAN: %v", err)
	}
	if err := second.Leaf.CheckSignatureFrom(c.Cert); err != nil {
		t.Fatalf("rotated cert must chain to the broker CA: %v", err)
	}
}

func TestServerCertSourceFailSoftOnMintFailure(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	src, err := c.NewServerCertSource("cn", nil, nil)
	if err != nil {
		t.Fatalf("NewServerCertSource: %v", err)
	}
	first, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	// Sabotage future mints, then enter the rotation margin while the cached
	// cert is still valid: the stale-but-valid cert must be served, not an error.
	src.ips = []string{"not-an-ip"}
	src.now = func() time.Time { return first.Leaf.NotAfter.Add(-rotateMargin / 2) }
	stale, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("mint failure with a still-valid cached cert must fail soft, got: %v", err)
	}
	if stale.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) != 0 {
		t.Fatal("fail-soft must serve the cached cert")
	}
	// Once genuinely expired, the mint failure must surface.
	src.now = func() time.Time { return first.Leaf.NotAfter.Add(time.Minute) }
	if _, err := src.GetCertificate(nil); err == nil {
		t.Fatal("expired cert + failing mint must return an error, not serve the dead cert")
	}
}

func TestServerCertSourceConcurrentHandshakes(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	src, err := c.NewServerCertSource("cn", nil, nil)
	if err != nil {
		t.Fatalf("NewServerCertSource: %v", err)
	}
	first, err := src.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	// Hold the clock inside the rotation margin so handshakes contend with
	// re-minting — makes -race exercise the lock for real.
	src.now = func() time.Time { return first.Leaf.NotAfter.Add(-rotateMargin / 2) }
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cert, err := src.GetCertificate(nil)
			if err != nil || cert == nil || cert.Leaf == nil {
				t.Errorf("concurrent GetCertificate: cert=%v err=%v", cert, err)
			}
		}()
	}
	wg.Wait()
}

func TestServerCertSourceFailsFastOnBadIP(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := c.NewServerCertSource("cn", nil, []string{"not-an-ip"}); err == nil {
		t.Fatal("constructor must fail fast on an invalid IP SAN (not defer to the first handshake)")
	}
}
