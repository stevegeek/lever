// Package webpush sends Web Push messages (RFC 8030) with the aes128gcm
// payload encryption of RFC 8291 and a VAPID signature (RFC 8292), on the
// standard library alone. lever sends only a fixed, content-free payload
// (see remoteproxy/push.go), but the push services still see only
// ciphertext: the subscription's own keys encrypt it.
package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// b64 is the encoding of every key and secret on the wire (RFC 8291 §2, RFC 8292 §3.2).
var b64 = base64.RawURLEncoding

const (
	// RecordSize is the record size the header names: one record carries
	// the whole payload (RFC 8188 §2.1).
	RecordSize = 4096
	// MaxPayload is the most plaintext one record holds: the record minus
	// the 16-byte tag and the 1-byte delimiter.
	MaxPayload = RecordSize - 17
	saltLen    = 16
	authLen    = 16
	pointLen   = 65 // an uncompressed P-256 point
	headerLen  = saltLen + 4 + 1 + pointLen
)

var (
	errPayloadTooLarge = errors.New("webpush: payload larger than one record")
	errAuthSecret      = errors.New("webpush: the auth secret must be 16 bytes")
	errBody            = errors.New("webpush: not an aes128gcm push body")
)

func hkdfExtract(salt, ikm []byte) []byte {
	m := hmac.New(sha256.New, salt)
	m.Write(ikm)
	return m.Sum(nil)
}

// hkdfExpand is HKDF-Expand for n ≤ 32: one HMAC block, which covers every
// length RFC 8291 asks for (32, 16, 12).
func hkdfExpand(prk, info []byte, n int) []byte {
	m := hmac.New(sha256.New, prk)
	m.Write(info)
	m.Write([]byte{1})
	return m.Sum(nil)[:n]
}

// contentKeys derives the content key and nonce (RFC 8291 §3.3-3.4, RFC 8188
// §2.2). uaPublic and asPublic go into the info in that order, whichever side
// derives them.
func contentKeys(secret, authSecret, uaPublic, asPublic, salt []byte) (cek, nonce []byte) {
	info := make([]byte, 0, 14+2*pointLen)
	info = append(info, "WebPush: info\x00"...)
	info = append(info, uaPublic...)
	info = append(info, asPublic...)
	ikm := hkdfExpand(hkdfExtract(authSecret, secret), info, 32)
	prk := hkdfExtract(salt, ikm)
	return hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16),
		hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12)
}

func gcm(cek []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// Encrypt encrypts plaintext for the subscription keys uaPublic (p256dh,
// 65 bytes) and authSecret (16 bytes), with a fresh sender key and salt.
func Encrypt(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	rand.Read(salt)
	return encrypt(uaPublic, authSecret, plaintext, as, salt)
}

// encrypt is Encrypt with the sender key and salt given (the RFC vector).
func encrypt(uaPublic, authSecret, plaintext []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, errPayloadTooLarge
	}
	if len(authSecret) != authLen {
		return nil, errAuthSecret
	}
	if len(uaPublic) != pointLen {
		return nil, fmt.Errorf("webpush: p256dh must be a %d-byte uncompressed point", pointLen)
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: p256dh: %w", err)
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	cek, nonce := contentKeys(secret, authSecret, uaPublic, asPublic, salt)
	aead, err := gcm(cek)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, headerLen+len(plaintext)+17)
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, RecordSize)
	out = append(out, byte(len(asPublic)))
	out = append(out, asPublic...)
	// The last (only) record ends with the delimiter 0x02 and no padding.
	padded := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 2)
	return aead.Seal(out, nonce, padded, nil), nil
}

// Decrypt is the user agent's side: the e2e receiver and the tests use it.
func Decrypt(ua *ecdh.PrivateKey, authSecret, body []byte) ([]byte, error) {
	if len(authSecret) != authLen {
		return nil, errAuthSecret
	}
	if len(body) < headerLen+17 {
		return nil, errBody
	}
	salt, rs, idLen := body[:saltLen], binary.BigEndian.Uint32(body[saltLen:saltLen+4]), body[saltLen+4]
	if idLen != pointLen || rs < 18 || len(body)-headerLen > int(rs) {
		return nil, errBody
	}
	asPublic := body[saltLen+5 : headerLen]
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, errBody
	}
	secret, err := ua.ECDH(as)
	if err != nil {
		return nil, errBody
	}
	cek, nonce := contentKeys(secret, authSecret, ua.PublicKey().Bytes(), asPublic, salt)
	aead, err := gcm(cek)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		return nil, errBody
	}
	i := len(pt) - 1
	for i >= 0 && pt[i] == 0 {
		i--
	}
	if i < 0 || pt[i] != 2 {
		return nil, errBody
	}
	return pt[:i], nil
}
