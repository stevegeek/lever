package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"strings"
	"time"
)

const jwtHeader = `{"typ":"JWT","alg":"ES256"}`

// MaxVAPIDLifetime is RFC 8292 §2's ceiling on exp.
const MaxVAPIDLifetime = 24 * time.Hour

type vapidClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}

// PublicKey is key's public point, uncompressed, base64url: the
// applicationServerKey the page subscribes with and the k= parameter.
func PublicKey(key *ecdsa.PrivateKey) (string, error) {
	b, err := key.PublicKey.Bytes()
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(b), nil
}

// VAPIDHeader is the Authorization value of one push request: a JWT for
// audience (the endpoint's origin) signed with key (ES256, raw r||s).
func VAPIDHeader(key *ecdsa.PrivateKey, audience, subject string, exp time.Time) (string, error) {
	claims, err := json.Marshal(vapidClaims{Aud: audience, Exp: exp.Unix(), Sub: subject})
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString([]byte(jwtHeader)) + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	pub, err := PublicKey(key)
	if err != nil {
		return "", err
	}
	return "vapid t=" + input + "." + b64.EncodeToString(sig) + ", k=" + pub, nil
}

var errVAPID = errors.New("webpush: VAPID header does not verify")

// VerifyVAPID is the push service's check, for the e2e receiver and tests:
// the token is signed by k, names audience, expires after now and within
// 24 hours, and names a subject.
func VerifyVAPID(header, audience string, now time.Time) error {
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		return errVAPID
	}
	tok, k, ok := strings.Cut(rest, ", k=")
	if !ok {
		return errVAPID
	}
	raw, err := b64.DecodeString(k)
	if err != nil {
		return errVAPID
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return errVAPID
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errVAPID
	}
	hdr, err1 := b64.DecodeString(parts[0])
	body, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || string(hdr) != jwtHeader || len(sig) != 64 {
		return errVAPID
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return errVAPID
	}
	var c vapidClaims
	if json.Unmarshal(body, &c) != nil || c.Aud != audience || c.Sub == "" {
		return errVAPID
	}
	exp := time.Unix(c.Exp, 0)
	if !exp.After(now) || exp.Sub(now) > MaxVAPIDLifetime {
		return errVAPID
	}
	return nil
}

// LoadOrCreateKey reads the VAPID key at path (base64url of the 32-byte
// scalar), creating it when absent. Its directory must exist.
func LoadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := ReadPrivateFile(path, 128)
	if errors.Is(err, fs.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		raw, err := key.Bytes()
		if err != nil {
			return nil, err
		}
		if err := WritePrivateFile(path, []byte(b64.EncodeToString(raw)+"\n")); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := b64.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("%s: not a VAPID key", path)
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("%s: not a VAPID key", path)
	}
	return key, nil
}
