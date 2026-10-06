package webpush

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

// RFC 8291 §5 (Appendix A) example.
const (
	vecPlaintext = "When I grow up, I want to be a watermelon"
	vecASPublic  = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	vecASPrivate = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	vecUAPublic  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	vecUAPrivate = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	vecSalt      = "DGv6ra1nlYgDCS1FRnbzlw"
	vecAuth      = "BTBZMqHH6r4Tts7J_aSIgg"
	vecECDH      = "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs"
	vecPRKKey    = "Snr3JMxaHVDXHWJn5wdC52WjpCtd2EIEGBykDcZW32k"
	vecIKM       = "S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg"
	vecPRK       = "09_eUZGrsvxChDCGRCdkLiDXrReGOEVeSCdCcPBSJSc"
	vecCEK       = "oIhVW04MRdy2XN9CiKLxTg"
	vecNonce     = "4h_95klXJ5E_qnoN"
	vecHeader    = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	vecCipher    = "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ"
	// vecBody is header || ciphertext, encoded as one value.
	vecBody = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func dec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func vecKeys(t *testing.T) (as, ua *ecdh.PrivateKey) {
	t.Helper()
	as, err := ecdh.P256().NewPrivateKey(dec(t, vecASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	ua, err = ecdh.P256().NewPrivateKey(dec(t, vecUAPrivate))
	if err != nil {
		t.Fatal(err)
	}
	return as, ua
}

// TestKeysRFC8291Vector checks every intermediate value of the example.
func TestKeysRFC8291Vector(t *testing.T) {
	as, ua := vecKeys(t)
	if got := b64.EncodeToString(as.PublicKey().Bytes()); got != vecASPublic {
		t.Fatalf("as_public %s", got)
	}
	if got := b64.EncodeToString(ua.PublicKey().Bytes()); got != vecUAPublic {
		t.Fatalf("ua_public %s", got)
	}
	secret, err := as.ECDH(ua.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got []byte, want string) {
		t.Helper()
		if g := b64.EncodeToString(got); g != want {
			t.Errorf("%s = %s, want %s", name, g, want)
		}
	}
	check("ecdh_secret", secret, vecECDH)
	prkKey := hkdfExtract(dec(t, vecAuth), secret)
	check("PRK_key", prkKey, vecPRKKey)
	info := append(append([]byte("WebPush: info\x00"), dec(t, vecUAPublic)...), dec(t, vecASPublic)...)
	ikm := hkdfExpand(prkKey, info, 32)
	check("IKM", ikm, vecIKM)
	check("PRK", hkdfExtract(dec(t, vecSalt), ikm), vecPRK)
	cek, nonce := contentKeys(secret, dec(t, vecAuth), dec(t, vecUAPublic), dec(t, vecASPublic), dec(t, vecSalt))
	check("CEK", cek, vecCEK)
	check("NONCE", nonce, vecNonce)
}

func TestEncryptRFC8291Vector(t *testing.T) {
	as, _ := vecKeys(t)
	got, err := encrypt(dec(t, vecUAPublic), dec(t, vecAuth), []byte(vecPlaintext), as, dec(t, vecSalt))
	if err != nil {
		t.Fatal(err)
	}
	if g := b64.EncodeToString(got[:headerLen]); g != vecHeader {
		t.Errorf("header %s", g)
	}
	if g := b64.EncodeToString(got[headerLen:]); g != vecCipher {
		t.Errorf("ciphertext %s", g)
	}
	if g := b64.EncodeToString(got); g != vecBody {
		t.Errorf("body %s", g)
	}
}

func TestDecryptRFC8291Vector(t *testing.T) {
	_, ua := vecKeys(t)
	got, err := Decrypt(ua, dec(t, vecAuth), dec(t, vecBody))
	if err != nil || string(got) != vecPlaintext {
		t.Fatalf("Decrypt = %q, %v", got, err)
	}
}

func TestEncryptRoundTripIsFreshEachTime(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	msg := []byte(`{"v":1,"agent":"deal-2"}`)
	a, err := Encrypt(ua.PublicKey().Bytes(), auth, msg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Encrypt(ua.PublicKey().Bytes(), auth, msg)
	if bytes.Equal(a, b) || bytes.Equal(a[:16], b[:16]) {
		t.Fatal("two encryptions share a salt or a body: the salt and the sender key must be fresh per message")
	}
	got, err := Decrypt(ua, auth, a)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

func TestEncryptRefusesBadInput(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	good := ua.PublicKey().Bytes()
	auth := make([]byte, 16)
	for name, tc := range map[string]struct {
		pub, auth, msg []byte
	}{
		"short auth":     {good, auth[:15], []byte("x")},
		"bad point":      {append([]byte{4}, make([]byte, 64)...), auth, []byte("x")},
		"compressed key": {good[:33], auth, []byte("x")},
		"too long":       {good, auth, make([]byte, MaxPayload+1)},
	} {
		if _, err := Encrypt(tc.pub, tc.auth, tc.msg); err == nil {
			t.Errorf("%s: Encrypt accepted it", name)
		}
	}
	if _, err := Encrypt(good, auth, make([]byte, MaxPayload)); err != nil {
		t.Errorf("a full record must fit: %v", err)
	}
}

func TestDecryptRefusesTampering(t *testing.T) {
	_, ua := vecKeys(t)
	body := dec(t, vecBody)
	for name, mutate := range map[string]func([]byte) []byte{
		"flipped tag byte": func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"flipped salt":     func(b []byte) []byte { b[0] ^= 1; return b },
		"short":            func(b []byte) []byte { return b[:headerLen+10] },
		"idlen 64":         func(b []byte) []byte { b[20] = 64; return b },
	} {
		if _, err := Decrypt(ua, dec(t, vecAuth), mutate(bytes.Clone(body))); err == nil {
			t.Errorf("%s: Decrypt accepted it", name)
		}
	}
}
