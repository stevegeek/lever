package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVAPIDHeaderShapeAndSignature(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	h, err := VAPIDHeader(key, "https://fcm.googleapis.com", "mailto:op@example.com", now.Add(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tok, k, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !strings.HasPrefix(h, "vapid t=") || !ok {
		t.Fatalf("header %q", h)
	}
	pub, _ := PublicKey(key)
	if k != pub || len(dec(t, k)) != 65 {
		t.Fatalf("k = %q, want the 65-byte public key %q", k, pub)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || string(dec(t, parts[0])) != `{"typ":"JWT","alg":"ES256"}` || len(dec(t, parts[2])) != 64 {
		t.Fatalf("token %q", tok)
	}
	var claims map[string]any
	if err := json.Unmarshal(dec(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "https://fcm.googleapis.com" || claims["sub"] != "mailto:op@example.com" ||
		claims["exp"] != float64(now.Add(12*time.Hour).Unix()) || len(claims) != 3 {
		t.Fatalf("claims %v", claims)
	}
	if err := VerifyVAPID(h, "https://fcm.googleapis.com", now); err != nil {
		t.Fatalf("own header does not verify: %v", err)
	}
}

func TestVerifyVAPIDRefuses(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	good, _ := VAPIDHeader(key, "https://a.push.apple.com", "mailto:x@y", now.Add(time.Hour))
	otherPub, _ := PublicKey(other)
	tooLong, _ := VAPIDHeader(key, "https://a.push.apple.com", "mailto:x@y", now.Add(25*time.Hour))
	expired, _ := VAPIDHeader(key, "https://a.push.apple.com", "mailto:x@y", now.Add(-time.Second))
	for name, h := range map[string]string{
		"wrong audience": good, // checked with another audience below
		"other key":      good[:strings.Index(good, ", k=")] + ", k=" + otherPub,
		"exp over 24h":   tooLong,
		"expired":        expired,
		"not vapid":      "Bearer " + good,
		"empty":          "",
	} {
		aud := "https://a.push.apple.com"
		if name == "wrong audience" {
			aud = "https://b.push.apple.com"
		}
		if err := VerifyVAPID(h, aud, now); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}
