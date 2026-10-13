package hubauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *Key {
	t.Helper()
	k, created, err := EnsureKey(filepath.Join(t.TempDir(), "state", "hub-auth.key"))
	if err != nil || !created {
		t.Fatalf("EnsureKey: created=%v err=%v", created, err)
	}
	return k
}

// verifyJWS checks a compact ES256 JWS with nothing but the standard library
// and returns its decoded header and claims.
func verifyJWS(t *testing.T, pub *ecdsa.PublicKey, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %q", tok)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: len=%d err=%v", len(sig), err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify as r||s")
	}
	dec := func(s string) map[string]any {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("segment: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("segment json: %v", err)
		}
		return m
	}
	return dec(parts[0]), dec(parts[1])
}

func fixedClock(host time.Time, offset time.Duration) Clock {
	return Clock{Offset: offset, now: func() time.Time { return host }}
}

func TestSignShapeAndSignature(t *testing.T) {
	k := testKey(t)
	host := time.Unix(1_800_000_000, 0)
	s := NewSigner(k, "abc123", fixedClock(host, 0))
	tok, err := s.Admin()
	if err != nil {
		t.Fatal(err)
	}
	hdr, c := verifyJWS(t, &k.priv.PublicKey, tok)
	if len(hdr) != 2 || hdr["alg"] != Algorithm || hdr["typ"] != "JWT" {
		t.Fatalf("header = %v", hdr)
	}
	want := map[string]any{"iss": "lever:abc123", "aud": "scion-hub:abc123", "sub": "lever-admin", "email": AdminEmail}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("claim %s = %v, want %v", k, c[k], v)
		}
	}
	if _, ok := c["aud"].(string); !ok {
		t.Errorf("aud must be a string, got %T", c["aud"])
	}
	jti, _ := c["jti"].(string)
	if b, err := base64.RawURLEncoding.DecodeString(jti); err != nil || len(b) != 16 {
		t.Errorf("jti %q: want 16 base64url bytes", jti)
	}
	tok2, _ := s.Admin()
	if _, c2 := verifyJWS(t, &k.priv.PublicKey, tok2); c2["jti"] == c["jti"] {
		t.Error("jti repeated")
	}
	other := testKey(t)
	parts := strings.Split(tok, ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if ecdsa.Verify(&other.priv.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("verified under another key")
	}

	ctl, err := s.Controller()
	if err != nil {
		t.Fatal(err)
	}
	if _, c := verifyJWS(t, &k.priv.PublicKey, ctl); c["email"] != ControllerEmail || c["sub"] != "lever-controller" {
		t.Errorf("controller claims = %v", c)
	}
}

func TestSignTimesFollowTheGuestClock(t *testing.T) {
	k := testKey(t)
	host := time.Unix(1_800_000_000, 0)
	for _, off := range []time.Duration{0, 60 * time.Second, -60 * time.Second} {
		_, c := verifyJWS(t, &k.priv.PublicKey, must(t)(NewSigner(k, "i", fixedClock(host, off)).Sign("a@b.c", "x")))
		iat, exp := int64(c["iat"].(float64)), int64(c["exp"].(float64))
		guest := host.Add(off).Unix()
		if iat != guest-5 || exp != guest+30 || exp-iat != 35 {
			t.Errorf("offset %v: iat=%d exp=%d guest=%d", off, iat, exp, guest)
		}
	}
	c := ClockFromGuestEpoch(1_800_000_045, time.Unix(1_800_000_000, 0))
	if c.Offset != 45*time.Second {
		t.Errorf("ClockFromGuestEpoch offset = %v", c.Offset)
	}
	if d := c.Now().Sub(time.Now()); d < 44*time.Second || d > 46*time.Second {
		t.Errorf("Now() is %v from host now", d)
	}
}

func must(t *testing.T) func(string, error) string {
	return func(s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

func TestSignLowercasesAndRefusesBadInput(t *testing.T) {
	k := testKey(t)
	s := NewSigner(k, "i", Clock{})
	_, c := verifyJWS(t, &k.priv.PublicKey, must(t)(s.Sign("Alice@Example.COM", "x")))
	if c["email"] != "alice@example.com" {
		t.Errorf("email = %v", c["email"])
	}
	for _, tc := range []struct{ email, sub string }{
		{"", "x"}, {"a@b.c ", "x"}, {" a@b.c", "x"}, {"a @b.c", "x"}, {"a@b.c\n", "x"},
		{"a\x00@b.c", "x"}, {"a\u200b@b.c", "x"}, {"\ufeffa@b.c", "x"}, {"a@b.c\u00a0", "x"},
		{"noat", "x"}, {"@b.c", "x"}, {"a@", "x"}, {"a@b.c", ""}, {"a@b.c", "x y"},
	} {
		if _, err := s.Sign(tc.email, tc.sub); err == nil {
			t.Errorf("Sign(%q, %q) succeeded", tc.email, tc.sub)
		}
	}
	if _, err := NewSigner(k, "", Clock{}).Admin(); err == nil {
		t.Error("signer without an instance id signed")
	}
	if _, err := NewSigner(nil, "i", Clock{}).Admin(); err == nil {
		t.Error("signer without a key signed")
	}
}

var reservedVariants = []string{
	"lever-admin@lever.local", "LEVER-ADMIN@LEVER.LOCAL", "Lever-Admin@Lever.Local", "lever-admin@lever.local ",
	" lever-controller@lever.local", "lever-controller@LEVER.local", "lever-admin@lever.local.",
	"X@LocalHost", "scion@localhost", "dev@localhost", "dev@LOCALHOST", "a@b@localhost",
	"dev@localho\u017ft", // U+017F folds to "s" under EqualFold
}

func TestIsReserved(t *testing.T) {
	for _, e := range reservedVariants {
		if !IsReserved(e) {
			t.Errorf("IsReserved(%q) = false", e)
		}
	}
	for _, e := range []string{"lever-operator@lever.local", "bob@id.lever.local", "alice@example.com",
		"localhost@example.com", "dev@localhost.example.com", "lever-admin@lever.locals", "nolocal"} {
		if IsReserved(e) {
			t.Errorf("IsReserved(%q) = true", e)
		}
	}
}

func TestRemoteSignerIsAPositiveAllowlist(t *testing.T) {
	k := testKey(t)
	s := NewSigner(k, "i", Clock{})
	r := NewRemoteSigner(s, append([]string{" Alice@Example.com ", "bob@id.lever.local"}, reservedVariants...))
	for _, e := range []string{"alice@example.com", "ALICE@example.com", "bob@id.lever.local"} {
		tok, err := r.Sign(e)
		if err != nil {
			t.Fatalf("Sign(%q): %v", e, err)
		}
		if _, c := verifyJWS(t, &k.priv.PublicKey, tok); c["sub"] != "lever-remote" || c["email"] != strings.ToLower(e) {
			t.Errorf("claims = %v", c)
		}
	}
	for _, e := range append([]string{"carol@example.com", "alice@example.com ", "", "lever-operator@lever.local"}, reservedVariants...) {
		if _, err := r.Sign(e); err == nil {
			t.Errorf("RemoteSigner signed %q", e)
		}
	}
}

func TestKeyFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hub-auth.key")
	if _, err := LoadKey(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadKey absent: %v", err)
	}
	k, created, err := EnsureKey(path)
	if err != nil || !created {
		t.Fatalf("EnsureKey: created=%v err=%v", created, err)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %#o (err %v)", p, fi.Mode().Perm(), want, err)
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Errorf("temp files left: %v", ents)
	}
	k2, created, err := EnsureKey(path)
	if err != nil || created || k2.Fingerprint() != k.Fingerprint() {
		t.Fatalf("second EnsureKey: created=%v err=%v", created, err)
	}
	k3, err := LoadKey(path)
	if err != nil || k3.Fingerprint() != k.Fingerprint() || !strings.HasPrefix(k.Fingerprint(), "sha256:") || len(k.Fingerprint()) != 7+64 {
		t.Fatalf("LoadKey: fp=%s err=%v", k.Fingerprint(), err)
	}

	block, _ := pem.Decode(k.PublicPEM())
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatal("PublicPEM is not a PUBLIC KEY block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if ep, ok := pub.(*ecdsa.PublicKey); err != nil || !ok || ep.Curve != elliptic.P256() {
		t.Fatalf("PublicPEM: %T %v", pub, err)
	}
	if sum := sha256.Sum256(block.Bytes); "sha256:"+hex.EncodeToString(sum[:]) != k.Fingerprint() {
		t.Error("fingerprint is not sha256 of the PKIX DER")
	}

	orig, _ := os.ReadFile(path)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureKey(path); err == nil {
		t.Fatal("0644 key accepted")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Error("EnsureKey re-moded the key")
	}
	if now, _ := os.ReadFile(path); string(now) != string(orig) {
		t.Error("EnsureKey replaced a refused key")
	}
	os.Chmod(path, 0o640)
	if _, err := LoadKey(path); err == nil {
		t.Fatal("0640 key accepted")
	}
	os.Chmod(path, 0o400)
	if _, err := LoadKey(path); err != nil {
		t.Fatalf("0400 key refused: %v", err)
	}
	os.Chmod(path, 0o600)

	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureKey(link); err == nil {
		t.Error("symlinked key accepted")
	}
	if _, _, err := EnsureKey(dir); err == nil {
		t.Error("directory accepted as key")
	}

	bad := func(name string, b []byte) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := EnsureKey(p); err == nil {
			t.Errorf("%s accepted", name)
		}
		if now, _ := os.ReadFile(p); string(now) != string(b) {
			t.Errorf("%s was rewritten", name)
		}
	}
	bad("empty.key", nil)
	bad("garbage.key", []byte("not a key"))
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(p384)
	bad("p384.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	bad("trailing.key", append(append([]byte{}, orig...), []byte("junk")...))
	bad("wrongtype.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: block.Bytes}))
}
